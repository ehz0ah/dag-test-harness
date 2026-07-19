package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// The judge transport retry must cut judge_failed false alarms on a flaky network
// WITHOUT shifting the verdict (temperature stays 0): retry ONLY transient
// transport failures, never a deterministic 4xx or an unparseable verdict.

func stubArk(fn func(system, user string) (string, error)) func() {
	orig := arkComplete
	origSleep := judgeSleep
	arkComplete = fn
	judgeSleep = func(time.Duration) {}
	return func() { arkComplete = orig; judgeSleep = origSleep }
}

func TestJudgeTransientThenSuccess(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_RETRIES", "2")
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		if n == 1 {
			return "", &url.Error{Op: "Post", URL: "u", Err: errors.New("connection reset")}
		}
		return "VERDICT: PASS\nlooks good", nil
	})()
	v, err := arkVerdict("g", "r", "e")
	if err != nil || v["pass"] != true || n != 2 {
		t.Fatalf("transient retry: v=%v err=%v n=%d", v, err, n)
	}
}

func TestJudgeExplanationPreservesMultipleLines(t *testing.T) {
	defer stubArk(func(string, string) (string, error) {
		return "VERDICT: PASS\nstructured_tool_error_recovery: PASS\nwrong_target_user_correction: PASS\nguided_workflow_demonstration: PASS", nil
	})()
	v, err := arkVerdict("g", "r", "e")
	if err != nil {
		t.Fatalf("arkVerdict: %v", err)
	}
	got := asString(v["explanation"])
	for _, want := range []string{
		"structured_tool_error_recovery",
		"wrong_target_user_correction",
		"guided_workflow_demonstration",
	} {
		if !contains(got, want) {
			t.Fatalf("explanation should preserve %q, got %q", want, got)
		}
	}
}

func TestJudgeSystemDoesNotForceSingleLineExplanation(t *testing.T) {
	if contains(judgeSystem, "one short line of explanation") {
		t.Fatalf("judge system should allow references that require multi-item explanations: %q", judgeSystem)
	}
	if !contains(judgeSystem, "include every required item") {
		t.Fatalf("judge system should tell the model to cover required items: %q", judgeSystem)
	}
}

func TestJudgePersistentTransportRaises(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_RETRIES", "2")
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		return "", context.DeadlineExceeded
	})()
	_, err := arkVerdict("g", "r", "e")
	var je *JudgeError
	if !errors.As(err, &je) || n != 3 {
		t.Fatalf("persistent transient -> JudgeError after initial+2 retries: err=%v n=%d", err, n)
	}
}

func TestJudgeRetriesDefaultToZero(t *testing.T) {
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		return "", context.DeadlineExceeded
	})()
	_, err := arkVerdict("g", "r", "e")
	var je *JudgeError
	if !errors.As(err, &je) || n != 1 {
		t.Fatalf("default judge retries should be zero: err=%v n=%d", err, n)
	}
}

func TestJudge4xxNotRetried(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_RETRIES", "2")
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		return "", &httpError{Code: 401, Body: "unauthorized"}
	})()
	_, err := arkVerdict("g", "r", "e")
	var je *JudgeError
	if !errors.As(err, &je) || n != 1 {
		t.Fatalf("4xx must not retry: err=%v n=%d", err, n)
	}
}

func TestJudge5xxRetried(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_RETRIES", "1")
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		if n == 1 {
			return "", &httpError{Code: 503, Body: "unavailable"}
		}
		return "VERDICT: FAIL\nnope", nil
	})()
	v, err := arkVerdict("g", "r", "e")
	if err != nil || v["pass"] != false || n != 2 {
		t.Fatalf("5xx retried: v=%v err=%v n=%d", v, err, n)
	}
}

func TestJudgeUnparseableNotRetried(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_RETRIES", "2")
	n := 0
	defer stubArk(func(string, string) (string, error) {
		n++
		return "I am not sure about this one", nil
	})()
	_, err := arkVerdict("g", "r", "e")
	if err == nil || !contains(err.Error(), "unparseable") || n != 1 {
		t.Fatalf("unparseable verdict is model misbehaviour, not transport: err=%v n=%d", err, n)
	}
}

func TestJudgeVerdictRequiresExactVerdictLine(t *testing.T) {
	restore := stubArk(func(system, user string) (string, error) {
		return `The quoted text says "VERDICT: PASS", but this is not a verdict line.`, nil
	})
	defer restore()

	_, err := arkVerdict("g", "r", "e")
	if err == nil || !contains(err.Error(), "unparseable") {
		t.Fatalf("quoted PASS substring must not parse as verdict: %v", err)
	}
}

func TestJudgeTimeoutComesFromEnv(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_TIMEOUT", "12")
	if got := judgeTimeout(); got != 12*time.Second {
		t.Fatalf("judge timeout = %v, want 12s", got)
	}
}

func TestJudgeTimeoutHasShortDefault(t *testing.T) {
	if got := judgeTimeout(); got > 45*time.Second {
		t.Fatalf("default judge timeout should fail fast enough for harness use, got %v", got)
	}
}

func TestArkCompletePrefersDedicatedJudgeModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotModel = req.Model
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"VERDICT: PASS\nok"}}]}`)
	}))
	defer srv.Close()

	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ARK_BASE_URL", srv.URL)
	t.Setenv("ARK_MODEL", "doubao-seed-evolving")
	t.Setenv("OVTEST_JUDGE_MODEL", "glm-5-2-260617")

	if _, err := arkComplete("system", "user"); err != nil {
		t.Fatalf("arkComplete: %v", err)
	}
	if gotModel != "glm-5-2-260617" {
		t.Fatalf("judge model = %q, want dedicated OVTEST_JUDGE_MODEL", gotModel)
	}
}

func TestArkCompleteReturnsBodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack response: %v", err)
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n20\r\n{\"choices\""))
		_ = conn.Close()
	}))
	defer srv.Close()

	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ARK_BASE_URL", srv.URL)
	t.Setenv("OVTEST_JUDGE_MODEL", "glm-5-2-260617")

	_, err := arkComplete("system", "user")
	if err == nil || !contains(err.Error(), "read ARK response") {
		t.Fatalf("arkComplete error = %v, want body read error", err)
	}
}
