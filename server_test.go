package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReservedEndpoints(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()

	testCases := []struct {
		path       string
		statusCode int
		bodyPart   string
	}{
		{path: "/help", statusCode: http.StatusOK, bodyPart: "https://github.com/Sov3rain/piping-server-go"},
		{path: "/version", statusCode: http.StatusOK, bodyPart: Version},
		{path: "/health", statusCode: http.StatusOK, bodyPart: "ok"},
		{path: "/", statusCode: http.StatusNotFound, bodyPart: "Not Found"},
	}

	for _, testCase := range testCases {
		req, err := http.NewRequest(http.MethodGet, ts.URL+testCase.path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request %s: %v", testCase.path, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != testCase.statusCode {
			t.Fatalf("unexpected status for %s: got %d want %d", testCase.path, resp.StatusCode, testCase.statusCode)
		}
		if !strings.Contains(body, testCase.bodyPart) {
			t.Fatalf("unexpected body for %s: %q", testCase.path, body)
		}

		headReq, err := http.NewRequest(http.MethodHead, ts.URL+testCase.path, nil)
		if err != nil {
			t.Fatalf("new head request: %v", err)
		}
		headResp, err := client.Do(headReq)
		if err != nil {
			t.Fatalf("do head request %s: %v", testCase.path, err)
		}
		_ = headResp.Body.Close()
		if headResp.StatusCode != testCase.statusCode {
			t.Fatalf("unexpected HEAD status for %s: got %d want %d", testCase.path, headResp.StatusCode, testCase.statusCode)
		}
		if headResp.Header.Get("Content-Length") != resp.Header.Get("Content-Length") {
			t.Fatalf("unexpected HEAD content-length for %s", testCase.path)
		}
	}
}

func TestOptionsRequest(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/hello", nil)
	if err != nil {
		t.Fatalf("new options request: %v", err)
	}
	req.Header.Set("Access-Control-Request-Private-Network", "true")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("options request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected options status: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("unexpected allow-origin: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "GET, HEAD, POST, OPTIONS" {
		t.Fatalf("unexpected allow-methods: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, Content-Disposition, X-Piping" {
		t.Fatalf("unexpected allow-headers: %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("unexpected allow-private-network: %q", got)
	}
}

func TestSenderFirstTransfer(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	postRespCh := make(chan *http.Response, 1)
	postErrCh := make(chan error, 1)

	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/hello", strings.NewReader("hello, world"))
		if err != nil {
			postErrCh <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			postErrCh <- err
			return
		}
		postRespCh <- resp
	}()

	time.Sleep(100 * time.Millisecond)

	getResp, err := client.Get(ts.URL + "/hello")
	if err != nil {
		t.Fatalf("get request failed: %v", err)
	}
	getBody := readBody(t, getResp)
	if getBody != "hello, world" {
		t.Fatalf("unexpected receiver body: %q", getBody)
	}

	select {
	case err := <-postErrCh:
		t.Fatalf("post request failed: %v", err)
	case postResp := <-postRespCh:
		postBody := readBody(t, postResp)
		if postResp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected sender status: %d", postResp.StatusCode)
		}
		if !strings.Contains(postBody, "Transfer completed to 1 receiver(s).") {
			t.Fatalf("unexpected sender body: %q", postBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sender response")
	}
}

func TestReceiverFirstTransfer(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	getRespCh := make(chan *http.Response, 1)
	getErrCh := make(chan error, 1)

	go func() {
		resp, err := client.Get(ts.URL + "/hello")
		if err != nil {
			getErrCh <- err
			return
		}
		getRespCh <- resp
	}()

	time.Sleep(100 * time.Millisecond)

	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/hello", strings.NewReader("hello, world"))
	if err != nil {
		t.Fatalf("new post request: %v", err)
	}
	postResp, err := client.Do(postReq)
	if err != nil {
		t.Fatalf("post request failed: %v", err)
	}

	select {
	case err := <-getErrCh:
		t.Fatalf("get request failed: %v", err)
	case getResp := <-getRespCh:
		getBody := readBody(t, getResp)
		if getBody != "hello, world" {
			t.Fatalf("unexpected receiver body: %q", getBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for receiver response")
	}

	postBody := readBody(t, postResp)
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected sender status: %d", postResp.StatusCode)
	}
	if !strings.Contains(postBody, "Transfer completed to 1 receiver(s).") {
		t.Fatalf("unexpected sender body: %q", postBody)
	}
}

func TestMultiReceiverTransfer(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()

	for _, receiverCount := range []int{2, 3} {
		bodyCh := make(chan string, receiverCount)
		errCh := make(chan error, receiverCount)

		for i := 0; i < receiverCount; i++ {
			go func() {
				resp, err := client.Get(ts.URL + "/fanout?n=" + strconv.Itoa(receiverCount))
				if err != nil {
					errCh <- err
					return
				}
				bodyCh <- readBody(t, resp)
			}()
		}

		time.Sleep(100 * time.Millisecond)

		postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/fanout?n="+strconv.Itoa(receiverCount), strings.NewReader("broadcast"))
		if err != nil {
			t.Fatalf("new post request: %v", err)
		}
		postResp, err := client.Do(postReq)
		if err != nil {
			t.Fatalf("post request failed: %v", err)
		}

		for i := 0; i < receiverCount; i++ {
			select {
			case err := <-errCh:
				t.Fatalf("receiver request failed: %v", err)
			case body := <-bodyCh:
				if body != "broadcast" {
					t.Fatalf("unexpected receiver body: %q", body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for receiver body")
			}
		}

		postBody := readBody(t, postResp)
		if postResp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected sender status for n=%d: %d", receiverCount, postResp.StatusCode)
		}
		if !strings.Contains(postBody, "Transfer completed to "+strconv.Itoa(receiverCount)+" receiver(s).") {
			t.Fatalf("unexpected sender body for n=%d: %q", receiverCount, postBody)
		}
	}
}

func TestStreamingChunkedTransfer(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	pipeReader, pipeWriter := io.Pipe()

	postRespCh := make(chan *http.Response, 1)
	postErrCh := make(chan error, 1)

	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stream", pipeReader)
		if err != nil {
			postErrCh <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			postErrCh <- err
			return
		}
		postRespCh <- resp
	}()

	getRespCh := make(chan *http.Response, 1)
	getErrCh := make(chan error, 1)
	go func() {
		resp, err := client.Get(ts.URL + "/stream")
		if err != nil {
			getErrCh <- err
			return
		}
		getRespCh <- resp
	}()

	var getResp *http.Response
	select {
	case err := <-getErrCh:
		t.Fatalf("get request failed: %v", err)
	case getResp = <-getRespCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for receiver response")
	}

	firstChunkCh := make(chan string, 1)
	readErrCh := make(chan error, 1)
	go func() {
		buf := make([]byte, len("hello "))
		if _, err := io.ReadFull(getResp.Body, buf); err != nil {
			readErrCh <- err
			return
		}
		firstChunkCh <- string(buf)
	}()

	if _, err := pipeWriter.Write([]byte("hello ")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}

	select {
	case err := <-readErrCh:
		t.Fatalf("failed reading first chunk: %v", err)
	case chunk := <-firstChunkCh:
		if chunk != "hello " {
			t.Fatalf("unexpected first chunk: %q", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not stream the first chunk before upload completion")
	}

	if _, err := pipeWriter.Write([]byte("world")); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	rest := readBody(t, getResp)
	if rest != "world" {
		t.Fatalf("unexpected remaining body: %q", rest)
	}

	select {
	case err := <-postErrCh:
		t.Fatalf("post request failed: %v", err)
	case postResp := <-postRespCh:
		postBody := readBody(t, postResp)
		if postResp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected sender status: %d", postResp.StatusCode)
		}
		if !strings.Contains(postBody, "Transfer completed to 1 receiver(s).") {
			t.Fatalf("unexpected sender body: %q", postBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sender response")
	}
}

func TestHeaderForwarding(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	getRespCh := make(chan *http.Response, 1)
	getErrCh := make(chan error, 1)

	go func() {
		resp, err := client.Get(ts.URL + "/headers")
		if err != nil {
			getErrCh <- err
			return
		}
		getRespCh <- resp
	}()

	time.Sleep(100 * time.Millisecond)

	postReq, err := http.NewRequest(http.MethodPost, ts.URL+"/headers", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new post request: %v", err)
	}
	postReq.Header.Set("Content-Type", "text/plain; charset=utf-8")
	postReq.Header.Set("Content-Disposition", `attachment; filename="hello.txt"`)
	postReq.Header.Add("X-Piping", "alpha")
	postReq.Header.Add("X-Piping", "beta")

	postResp, err := client.Do(postReq)
	if err != nil {
		t.Fatalf("post request failed: %v", err)
	}

	select {
	case err := <-getErrCh:
		t.Fatalf("get request failed: %v", err)
	case getResp := <-getRespCh:
		if got := getResp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("unexpected content-type: %q", got)
		}
		if got := getResp.Header.Get("Content-Disposition"); got != `attachment; filename="hello.txt"` {
			t.Fatalf("unexpected content-disposition: %q", got)
		}
		values := getResp.Header.Values("X-Piping")
		if len(values) != 2 || values[0] != "alpha" || values[1] != "beta" {
			t.Fatalf("unexpected x-piping values: %#v", values)
		}
		if body := readBody(t, getResp); body != "payload" {
			t.Fatalf("unexpected receiver body: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for receiver response")
	}

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected sender status: %d", postResp.StatusCode)
	}
	_ = readBody(t, postResp)
}

func TestErrorCases(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()

	deleteReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/nope", nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	deleteResp, err := client.Do(deleteReq)
	if err != nil {
		t.Fatalf("delete request failed: %v", err)
	}
	if deleteResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected delete status: %d", deleteResp.StatusCode)
	}
	_ = readBody(t, deleteResp)

	postReservedReq, err := http.NewRequest(http.MethodPost, ts.URL+"/help", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new reserved post request: %v", err)
	}
	postReservedResp, err := client.Do(postReservedReq)
	if err != nil {
		t.Fatalf("reserved post request failed: %v", err)
	}
	if postReservedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected reserved post status: %d", postReservedResp.StatusCode)
	}
	_ = readBody(t, postReservedResp)

	invalidNReq, err := http.NewRequest(http.MethodPost, ts.URL+"/invalid?n=nope", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new invalid n request: %v", err)
	}
	invalidNResp, err := client.Do(invalidNReq)
	if err != nil {
		t.Fatalf("invalid n request failed: %v", err)
	}
	if invalidNResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected invalid n status: %d", invalidNResp.StatusCode)
	}
	_ = readBody(t, invalidNResp)

	firstSenderRespCh := make(chan *http.Response, 1)
	firstSenderErrCh := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/dup?n=1", strings.NewReader("payload"))
		if err != nil {
			firstSenderErrCh <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			firstSenderErrCh <- err
			return
		}
		firstSenderRespCh <- resp
	}()

	time.Sleep(100 * time.Millisecond)

	secondSenderReq, err := http.NewRequest(http.MethodPost, ts.URL+"/dup?n=1", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new second sender request: %v", err)
	}
	secondSenderResp, err := client.Do(secondSenderReq)
	if err != nil {
		t.Fatalf("second sender request failed: %v", err)
	}
	if secondSenderResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected second sender status: %d", secondSenderResp.StatusCode)
	}
	_ = readBody(t, secondSenderResp)

	receiverResp, err := client.Get(ts.URL + "/dup?n=1")
	if err != nil {
		t.Fatalf("cleanup receiver request failed: %v", err)
	}
	if body := readBody(t, receiverResp); body != "payload" {
		t.Fatalf("unexpected cleanup receiver body: %q", body)
	}

	select {
	case err := <-firstSenderErrCh:
		t.Fatalf("first sender failed: %v", err)
	case firstSenderResp := <-firstSenderRespCh:
		if firstSenderResp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected first sender status: %d", firstSenderResp.StatusCode)
		}
		_ = readBody(t, firstSenderResp)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first sender response")
	}

	firstReceiverCh := make(chan *http.Response, 1)
	firstReceiverErrCh := make(chan error, 1)
	go func() {
		resp, err := client.Get(ts.URL + "/full?n=1")
		if err != nil {
			firstReceiverErrCh <- err
			return
		}
		firstReceiverCh <- resp
	}()

	time.Sleep(100 * time.Millisecond)

	secondReceiverResp, err := client.Get(ts.URL + "/full?n=1")
	if err != nil {
		t.Fatalf("second receiver request failed: %v", err)
	}
	if secondReceiverResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected second receiver status: %d", secondReceiverResp.StatusCode)
	}
	_ = readBody(t, secondReceiverResp)

	fullPostReq, err := http.NewRequest(http.MethodPost, ts.URL+"/full?n=1", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new cleanup post request: %v", err)
	}
	fullPostResp, err := client.Do(fullPostReq)
	if err != nil {
		t.Fatalf("cleanup post request failed: %v", err)
	}
	if fullPostResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected cleanup post status: %d", fullPostResp.StatusCode)
	}
	_ = readBody(t, fullPostResp)

	select {
	case err := <-firstReceiverErrCh:
		t.Fatalf("first receiver failed: %v", err)
	case firstReceiverResp := <-firstReceiverCh:
		if body := readBody(t, firstReceiverResp); body != "payload" {
			t.Fatalf("unexpected first receiver body: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first receiver")
	}
}

func TestReceiverAbortReturnsSenderError(t *testing.T) {
	ts := httptest.NewServer(NewServer(Version))
	defer ts.Close()

	client := ts.Client()
	pipeReader, pipeWriter := io.Pipe()
	postRespCh := make(chan *http.Response, 1)
	postErrCh := make(chan error, 1)

	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/abort", pipeReader)
		if err != nil {
			postErrCh <- err
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			postErrCh <- err
			return
		}
		postRespCh <- resp
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/abort", nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatalf("get request failed: %v", err)
	}

	if _, err := pipeWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}

	buf := make([]byte, len("hello"))
	if _, err := io.ReadFull(getResp.Body, buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("unexpected first chunk: %q", string(buf))
	}

	_ = getResp.Body.Close()
	cancel()

	if _, err := pipeWriter.Write([]byte(strings.Repeat("x", 64*1024))); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	select {
	case err := <-postErrCh:
		t.Fatalf("post request failed: %v", err)
	case postResp := <-postRespCh:
		postBody := readBody(t, postResp)
		if postResp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("unexpected sender status: %d", postResp.StatusCode)
		}
		if !strings.Contains(postBody, "All receiver(s) aborted before completion.") {
			t.Fatalf("unexpected sender body: %q", postBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sender response")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
