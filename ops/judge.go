package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// judge: the ARK LLM verdict primitive — the only LLM in the framework. arkVerdict
// returns {pass, explanation}, or a *JudgeError if the model call or verdict parse
// fails (a FAIL verdict is data, not an error). A bounded transport retry cuts
// judge_failed false alarms on a flaky network without shifting the verdict
// (temperature stays 0); a 4xx or an unparseable verdict is never retried.

const judgeSystem = "You are a strict end-to-end test judge. You are given a test goal, a " +
	"human-authored natural-language reference describing the intended outcome, and the evidence " +
	"captured during a deterministic run. Decide whether the run satisfied the reference. Judge ONLY " +
	"from the evidence provided. Reply with exactly one line 'VERDICT: PASS' or 'VERDICT: FAIL', " +
	"then a concise explanation. If the reference requires named scenarios, checks, or cases, include every required item."

type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

// arkComplete posts one chat completion to ARK. Package var so tests stub it.
var arkComplete = func(system, user string) (string, error) {
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		return "", errors.New("ARK_API_KEY not set")
	}
	model, err := judgeModel()
	if err != nil {
		return "", err
	}
	base := envStr("ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3")
	body, _ := json.Marshal(map[string]any{
		"model": model, "temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequest("POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: judgeTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("read ARK response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpError{Code: resp.StatusCode, Body: string(data)}
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", fmt.Errorf("unexpected ARK response: %s", string(data))
	}
	return parsed.Choices[0].Message.Content, nil
}

func judgeModel() (string, error) {
	if model := os.Getenv("OVTEST_JUDGE_MODEL"); model != "" {
		return model, nil
	}
	if model := os.Getenv("ARK_MODEL"); model != "" {
		return model, nil
	}
	return "", errors.New("OVTEST_JUDGE_MODEL or ARK_MODEL not set")
}

// judgeSleep is the retry backoff; a package var so tests make it a no-op.
var judgeSleep = func(d time.Duration) { time.Sleep(d) }

func judgeTimeout() time.Duration {
	seconds := envInt("OVTEST_JUDGE_TIMEOUT", 30)
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

// isTransient reports a TRANSPORT failure worth one more try: HTTP 429/5xx, a
// connection error, or a timeout. A 4xx (bad/expired key, quota) is deterministic.
func isTransient(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Code == 429 || (he.Code >= 500 && he.Code < 600)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

func explanation(raw string) string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(strings.ToUpper(line), "VERDICT:") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// arkVerdict returns {pass, explanation} or a *JudgeError.
func arkVerdict(goal, reference, evidence string) (map[string]any, error) {
	user := fmt.Sprintf("GOAL:\n%s\n\nREFERENCE (intended outcome):\n%s\n\n%s", goal, reference, evidence)
	retries := envInt("OVTEST_JUDGE_RETRIES", 0)
	if retries < 0 {
		retries = 0
	}
	var raw string
	for attempt := 0; ; attempt++ {
		r, err := arkComplete(judgeSystem, user)
		if err == nil {
			raw = r
			break
		}
		if attempt < retries && isTransient(err) {
			backoff := 1 << attempt
			if backoff > 8 {
				backoff = 8
			}
			judgeSleep(time.Duration(backoff) * time.Second)
			continue
		}
		return nil, &JudgeError{Detail: "ARK call failed: " + err.Error()}
	}
	pass, ok := parseVerdictLine(raw)
	if ok && pass {
		return map[string]any{"pass": true, "explanation": explanation(raw)}, nil
	}
	if ok && !pass {
		return map[string]any{"pass": false, "explanation": explanation(raw)}, nil
	}
	return nil, &JudgeError{Detail: fmt.Sprintf("unparseable verdict: %q", raw)}
}

func parseVerdictLine(raw string) (bool, bool) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "VERDICT:") {
			continue
		}
		value := strings.TrimSpace(line[len("VERDICT:"):])
		switch strings.ToUpper(value) {
		case "PASS":
			return true, true
		case "FAIL":
			return false, true
		}
	}
	return false, false
}
