package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+$`)
var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var uploadIDPattern = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

func runUpload(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("upload-sarif", flag.ContinueOnError)
	repo := flags.String("repo", os.Getenv("GITHUB_REPOSITORY"), "GitHub owner/repository")
	ref := flags.String("ref", os.Getenv("GITHUB_REF"), "full Git ref")
	commit := flags.String("commit", os.Getenv("GITHUB_SHA"), "analyzed commit SHA")
	file := flags.String("file", "", "SARIF file")
	category := flags.String("category", "mse", "analysis category")
	timeout := flags.Duration("timeout", 2*time.Minute, "maximum time for upload and processing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *file == "" || !repositoryPattern.MatchString(*repo) || !commitPattern.MatchString(*commit) || !strings.HasPrefix(*ref, "refs/") || strings.TrimSpace(*category) == "" || strings.Contains(*category, "/") || *timeout <= 0 {
		return errors.New("usage: mse upload-sarif --file RESULTS.sarif --repo OWNER/REPO --ref refs/heads/BRANCH --commit SHA [--category NAME]")
	}
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return errors.New("set GH_TOKEN or GITHUB_TOKEN")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	payload, err := uploadPayload(data, *ref, *commit, *category)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return uploadSARIF(ctx, client, "https://api.github.com/repos/"+*repo+"/code-scanning/sarifs", token, payload, out, 2*time.Second)
}

func uploadPayload(data []byte, ref, commit, category string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var version string
	var runs []map[string]json.RawMessage
	if json.Unmarshal(doc["version"], &version) != nil || version != "2.1.0" || json.Unmarshal(doc["runs"], &runs) != nil || len(runs) != 1 || runs[0] == nil {
		return nil, errors.New("upload requires SARIF 2.1.0 with exactly one run")
	}
	runs[0]["automationDetails"], _ = json.Marshal(map[string]string{"id": category + "/"})
	doc["runs"], _ = json.Marshal(runs)
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	if err := json.NewEncoder(zipper).Encode(doc); err != nil {
		return nil, err
	}
	if err := zipper.Close(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"commit_sha": commit, "ref": ref, "sarif": base64.StdEncoding.EncodeToString(compressed.Bytes()), "validate": true})
}

func uploadSARIF(ctx context.Context, client *http.Client, endpoint, token string, payload []byte, out io.Writer, interval time.Duration) error {
	request := func(method, address string, body []byte, want int, result any) error {
		req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		reader := io.LimitReader(resp.Body, 64<<10)
		if resp.StatusCode != want {
			var message struct{ Message string }
			_ = json.NewDecoder(reader).Decode(&message)
			return fmt.Errorf("GitHub %s returned HTTP %d: %s", method, resp.StatusCode, message.Message)
		}
		return json.NewDecoder(reader).Decode(result)
	}
	var accepted struct{ ID string }
	if err := request(http.MethodPost, endpoint, payload, http.StatusAccepted, &accepted); err != nil {
		return err
	}
	if !uploadIDPattern.MatchString(accepted.ID) {
		return errors.New("GitHub returned invalid upload ID")
	}
	fmt.Fprintf(out, "SARIF upload ID: %s\n", accepted.ID)
	for {
		var state struct {
			Status      string   `json:"processing_status"`
			Errors      []string `json:"errors"`
			AnalysesURL string   `json:"analyses_url"`
		}
		if err := request(http.MethodGet, endpoint+"/"+accepted.ID, nil, http.StatusOK, &state); err != nil {
			return fmt.Errorf("upload %s: %w", accepted.ID, err)
		}
		switch state.Status {
		case "complete":
			fmt.Fprintf(out, "SARIF processing complete: %s\n", state.AnalysesURL)
			return nil
		case "failed":
			return fmt.Errorf("SARIF processing failed: %s", strings.Join(state.Errors, "; "))
		case "pending":
		default:
			return fmt.Errorf("unexpected SARIF processing status %q", state.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("upload %s: %w", accepted.ID, ctx.Err())
		case <-time.After(interval):
		}
	}
}
