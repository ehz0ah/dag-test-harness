package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type apiRequest struct {
	Method  string
	URL     string
	Path    string
	Headers map[string]string
	Body    []byte
	Timeout int
}

var runAPI = func(req apiRequest) cliResult {
	if req.Timeout <= 0 {
		req.Timeout = envInt("OV_TEST_API_TIMEOUT", 300)
	}
	httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return cliResult{Cmd: req.Method + " " + req.Path, ExitCode: 127, Stderr: err.Error()}
	}
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	client := &http.Client{Timeout: time.Duration(req.Timeout) * time.Second}
	t0 := time.Now()
	resp, err := client.Do(httpReq)
	dur := round2(time.Since(t0).Seconds())
	if err != nil {
		return cliResult{Cmd: req.Method + " " + req.Path, ExitCode: 127, Stderr: err.Error(), DurationS: dur}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return cliResult{Cmd: req.Method + " " + req.Path, ExitCode: 127, Stderr: readErr.Error(), DurationS: dur}
	}
	exit := 0
	stderr := ""
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		exit = resp.StatusCode
		stderr = strings.TrimSpace(string(body))
	}
	return cliResult{
		Cmd:       fmt.Sprintf("%s %s", req.Method, req.Path),
		ExitCode:  exit,
		Stdout:    string(body),
		Stderr:    stderr,
		DurationS: dur,
	}
}

func runAsAPI(node string, userKey any, method, path string, body any, settle int) (cliResult, error) {
	if settle > 0 {
		time.Sleep(time.Duration(settle) * time.Second)
	}
	confPath, err := userConf(node, userKey)
	if err != nil {
		return cliResult{}, err
	}
	conf, err := ReadCLIConf(confPath)
	if err != nil {
		return cliResult{}, configErr(node, "could not read OpenViking config: "+err.Error())
	}
	baseURL := strings.TrimRight(asString(conf["url"]), "/")
	if baseURL == "" {
		return cliResult{}, configErr(node, fmt.Sprintf("%s has no url", confPath))
	}
	apiKey := asString(conf["api_key"])
	if apiKey == "" {
		return cliResult{}, configErr(node, fmt.Sprintf("%s has no api_key", confPath))
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	raw := []byte("{}")
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return cliResult{}, configErr(node, "could not encode request body: "+err.Error())
		}
	}
	return runAPI(apiRequest{
		Method: method,
		URL:    baseURL + path,
		Path:   path,
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"X-API-Key":     apiKey,
			"Authorization": "Bearer " + apiKey,
		},
		Body:    raw,
		Timeout: envInt("OV_TEST_API_TIMEOUT", 300),
	}), nil
}
