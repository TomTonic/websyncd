package syncer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type Syncer struct {
	Client     HTTPDoer
	Resource   string
	OutputPath string

	etag         string
	lastModified string
}

func (s *Syncer) Sync(ctx context.Context) error {
	if s.Client == nil {
		return fmt.Errorf("http client is required")
	}

	headResp, needGET, err := s.head(ctx)
	if err == nil && headResp != nil {
		defer func() { _ = headResp.Body.Close() }()
	}
	if err != nil {
		needGET = true
	}
	if !needGET {
		return nil
	}

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Resource, nil)
	if err != nil {
		return err
	}
	getResp, err := s.Client.Do(getReq)
	if err != nil {
		return err
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode == http.StatusNotModified {
		return nil
	}
	if getResp.StatusCode < 200 || getResp.StatusCode >= 300 {
		return fmt.Errorf("GET %s failed: %s", s.Resource, getResp.Status)
	}

	if err := s.writeAtomically(getResp.Body); err != nil {
		return err
	}

	s.etag = strings.TrimSpace(getResp.Header.Get("ETag"))
	s.lastModified = strings.TrimSpace(getResp.Header.Get("Last-Modified"))
	return nil
}

func (s *Syncer) head(ctx context.Context) (*http.Response, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.Resource, nil)
	if err != nil {
		return nil, false, err
	}
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	if s.lastModified != "" {
		req.Header.Set("If-Modified-Since", s.lastModified)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return resp, true, nil
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, true, nil
	}

	headETag := strings.TrimSpace(resp.Header.Get("ETag"))
	headLM := strings.TrimSpace(resp.Header.Get("Last-Modified"))

	if s.etag != "" && headETag != "" && s.etag == headETag {
		return resp, false, nil
	}
	if s.lastModified != "" && headLM != "" && s.lastModified == headLM {
		return resp, false, nil
	}

	return resp, true, nil
}

func (s *Syncer) writeAtomically(body io.Reader) error {
	dir := filepath.Dir(s.OutputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".websyncd-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = io.Copy(tmp, body); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, s.OutputPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}
