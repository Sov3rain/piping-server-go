package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

const Version = "1.0.0"

type senderResult struct {
	status int
	body   string
}

type senderConn struct {
	req      *http.Request
	w        http.ResponseWriter
	resultCh chan *senderResult
	once     sync.Once
}

func newSenderConn(w http.ResponseWriter, req *http.Request) *senderConn {
	return &senderConn{
		req:      req,
		w:        w,
		resultCh: make(chan *senderResult, 1),
	}
}

func (s *senderConn) complete(result *senderResult) {
	s.once.Do(func() {
		if result != nil {
			s.resultCh <- result
		}
		close(s.resultCh)
	})
}

type receiverConn struct {
	req  *http.Request
	w    http.ResponseWriter
	done chan struct{}
	once sync.Once
}

func newReceiverConn(w http.ResponseWriter, req *http.Request) *receiverConn {
	return &receiverConn{
		req:  req,
		w:    w,
		done: make(chan struct{}),
	}
}

func (r *receiverConn) complete() {
	r.once.Do(func() {
		close(r.done)
	})
}

type session struct {
	path              string
	expectedReceivers int
	sender            *senderConn
	receivers         []*receiverConn
	established       bool
}

type Server struct {
	mu       sync.Mutex
	sessions map[string]*session
	logger   *log.Logger
	version  string
}

func NewServer(version string) *Server {
	if version == "" {
		version = Version
	}
	return &Server{
		sessions: make(map[string]*session),
		logger:   log.New(os.Stdout, "", log.LstdFlags),
		version:  version,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	if path == "" {
		path = "/"
	}

	s.logger.Printf("%s %s", req.Method, req.URL.String())

	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		if s.handleReserved(w, req, path, req.Method == http.MethodHead) {
			return
		}
	}

	switch req.Method {
	case http.MethodPost:
		if isReservedPath(path) {
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] Cannot send to the reserved path '%s'. (e.g. '/mypath123')\n", path), false)
			return
		}
		if req.Header.Get("Content-Range") != "" {
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), "[ERROR] Content-Range is not supported for now in POST\n", false)
			return
		}
		s.handleSender(w, req)
	case http.MethodGet:
		s.handleReceiver(w, req)
	case http.MethodHead:
		writeMethodNotAllowed(w, req.Method)
	case http.MethodOptions:
		s.handleOptions(w, req)
	default:
		writeMethodNotAllowed(w, req.Method)
	}
}

func (s *Server) handleReserved(w http.ResponseWriter, req *http.Request, path string, head bool) bool {
	switch path {
	case "/help":
		writeText(w, http.StatusOK, textHeaders("text/plain"), generateHelpPage(baseURL(req), s.version), head)
		return true
	case "/version":
		writeText(w, http.StatusOK, textHeaders("text/plain"), s.version+"\n", head)
		return true
	case "/health":
		writeText(w, http.StatusOK, textHeaders("text/plain"), "ok\n", head)
		return true
	case "/", "/noscript", "/favicon.ico", "/robots.txt":
		writeText(w, http.StatusNotFound, textHeaders("text/plain"), "Not Found\n", head)
		return true
	default:
		return false
	}
}

func (s *Server) handleOptions(w http.ResponseWriter, req *http.Request) {
	headers := map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "GET, HEAD, POST, OPTIONS",
		"Access-Control-Allow-Headers":  "Content-Type, Content-Disposition, X-Piping",
		"Access-Control-Expose-Headers": "Access-Control-Allow-Headers",
		"Access-Control-Max-Age":        "86400",
		"Content-Length":                "0",
	}
	if req.Header.Get("Access-Control-Request-Private-Network") == "true" {
		headers["Access-Control-Allow-Private-Network"] = "true"
	}
	writeHeaders(w, http.StatusOK, headers)
}

func (s *Server) handleSender(w http.ResponseWriter, req *http.Request) {
	nReceivers, err := parseReceiverCount(req)
	if err != nil {
		writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), err.Error(), false)
		return
	}

	path := req.URL.Path
	sender := newSenderConn(w, req)

	shouldStart := false

	s.mu.Lock()
	current := s.sessions[path]
	if current == nil {
		current = &session{
			path:              path,
			expectedReceivers: nReceivers,
		}
		s.sessions[path] = current
	} else {
		if current.established {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] Connection on '%s' has been established already.\n", path), false)
			return
		}
		if current.expectedReceivers != nReceivers {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] The number of receivers should be %d but %d.\n", current.expectedReceivers, nReceivers), false)
			return
		}
		if current.sender != nil {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] Another sender has been connected on '%s'.\n", path), false)
			return
		}
	}

	current.sender = sender
	if len(current.receivers) == current.expectedReceivers {
		current.established = true
		shouldStart = true
	}
	s.mu.Unlock()

	go s.watchWaitingSender(path, sender)

	if shouldStart {
		go s.runTransfer(current)
	}

	result, ok := <-sender.resultCh
	if !ok || result == nil {
		return
	}
	writeText(w, result.status, senderAndReceiverHeaders(), result.body, false)
}

func (s *Server) handleReceiver(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Service-Worker") == "script" {
		writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), "[ERROR] Service Worker registration is rejected.\n", false)
		return
	}

	nReceivers, err := parseReceiverCount(req)
	if err != nil {
		writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), err.Error(), false)
		return
	}

	path := req.URL.Path
	receiver := newReceiverConn(w, req)

	shouldStart := false

	s.mu.Lock()
	current := s.sessions[path]
	if current == nil {
		current = &session{
			path:              path,
			expectedReceivers: nReceivers,
		}
		s.sessions[path] = current
	} else {
		if current.established {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] Connection on '%s' has been established already.\n", path), false)
			return
		}
		if current.expectedReceivers != nReceivers {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), fmt.Sprintf("[ERROR] The number of receivers should be %d but %d.\n", current.expectedReceivers, nReceivers), false)
			return
		}
		if len(current.receivers) == current.expectedReceivers {
			s.mu.Unlock()
			writeText(w, http.StatusBadRequest, senderAndReceiverHeaders(), "[ERROR] The number of receivers has reached limits.\n", false)
			return
		}
	}

	current.receivers = append(current.receivers, receiver)
	if current.sender != nil && len(current.receivers) == current.expectedReceivers {
		current.established = true
		shouldStart = true
	}
	s.mu.Unlock()

	go s.watchWaitingReceiver(path, receiver)

	if shouldStart {
		go s.runTransfer(current)
	}

	<-receiver.done
}

func (s *Server) watchWaitingSender(path string, sender *senderConn) {
	<-sender.req.Context().Done()

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.sessions[path]
	if current == nil || current.established || current.sender != sender {
		return
	}

	current.sender = nil
	if len(current.receivers) == 0 {
		delete(s.sessions, path)
	}
	sender.complete(nil)
}

func (s *Server) watchWaitingReceiver(path string, receiver *receiverConn) {
	<-receiver.req.Context().Done()

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.sessions[path]
	if current == nil || current.established {
		return
	}

	index := -1
	for i, currentReceiver := range current.receivers {
		if currentReceiver == receiver {
			index = i
			break
		}
	}
	if index == -1 {
		return
	}

	current.receivers = append(current.receivers[:index], current.receivers[index+1:]...)
	if current.sender == nil && len(current.receivers) == 0 {
		delete(s.sessions, path)
	}
	receiver.complete()
}

func (s *Server) runTransfer(current *session) {
	defer s.cleanupSession(current.path, current)
	defer senderBodyClose(current.sender.req)

	sender := current.sender
	receivers := append([]*receiverConn(nil), current.receivers...)

	for _, receiver := range receivers {
		writeTransferHeaders(receiver.w, sender.req.Header)
	}

	activeReceivers := append([]*receiverConn(nil), receivers...)
	abortedCount := 0
	buffer := make([]byte, 32*1024)

	for {
		n, err := sender.req.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			nextActive := activeReceivers[:0]
			for _, receiver := range activeReceivers {
				if receiver.req.Context().Err() != nil {
					abortedCount++
					receiver.complete()
					continue
				}
				if writeChunk(receiver.w, chunk) {
					nextActive = append(nextActive, receiver)
					continue
				}
				abortedCount++
				receiver.complete()
			}
			activeReceivers = nextActive

			if len(activeReceivers) == 0 {
				if _, drainErr := io.Copy(io.Discard, sender.req.Body); drainErr != nil {
					s.logger.Printf("failed to drain sender body for %s: %v", current.path, drainErr)
				}
				sender.complete(&senderResult{
					status: http.StatusInternalServerError,
					body:   "[ERROR] All receiver(s) aborted before completion.\n",
				})
				return
			}
		}

		if err == nil {
			continue
		}

		if err == io.EOF {
			completedReceivers := activeReceivers[:0]
			for _, receiver := range activeReceivers {
				if receiver.req.Context().Err() != nil {
					abortedCount++
					receiver.complete()
					continue
				}
				receiver.complete()
				completedReceivers = append(completedReceivers, receiver)
			}

			completedCount := len(completedReceivers)
			if abortedCount == 0 {
				sender.complete(&senderResult{
					status: http.StatusOK,
					body:   fmt.Sprintf("[INFO] Transfer completed to %d receiver(s).\n", completedCount),
				})
				return
			}

			sender.complete(&senderResult{
				status: http.StatusInternalServerError,
				body:   fmt.Sprintf("[ERROR] Transfer completed for %d/%d receiver(s).\n", completedCount, current.expectedReceivers),
			})
			return
		}

		for _, receiver := range activeReceivers {
			receiver.complete()
		}
		sender.complete(&senderResult{
			status: http.StatusInternalServerError,
			body:   "[ERROR] Failed to send.\n",
		})
		return
	}
}

func (s *Server) cleanupSession(path string, current *session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessions[path] == current {
		delete(s.sessions, path)
	}
}

func parseReceiverCount(req *http.Request) (int, error) {
	raw := req.URL.Query().Get("n")
	if raw == "" {
		return 1, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("[ERROR] Invalid \"n\" query parameter\n")
	}
	if n <= 0 {
		return 0, fmt.Errorf("[ERROR] n should > 0, but n = %d.\n", n)
	}
	return n, nil
}

func baseURL(req *http.Request) string {
	scheme := "http"
	if req.TLS != nil || strings.Contains(strings.ToLower(req.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}

	host := req.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

func generateHelpPage(url string, version string) string {
	return fmt.Sprintf(`Help for Piping Server %s
(Repository: https://github.com/Sov3rain/piping-server-go)

======= Get  =======
curl %s/mypath

======= Send =======
# Send a file
curl -X POST --data-binary @myfile %s/mypath

# Send a text
echo 'hello!' | curl -X POST --data-binary @- %s/mypath

# Send to multiple receivers
curl -X POST --data-binary @myfile "%s/mypath?n=3"
`, version, url, url, url, url)
}

func isReservedPath(path string) bool {
	switch path {
	case "/", "/help", "/version", "/health", "/noscript", "/favicon.ico", "/robots.txt":
		return true
	default:
		return false
	}
}

func senderAndReceiverHeaders() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Content-Type":                "text/plain",
	}
}

func textHeaders(contentType string) map[string]string {
	return map[string]string{
		"Content-Type": contentType,
	}
}

func writeMethodNotAllowed(w http.ResponseWriter, method string) {
	writeText(w, http.StatusMethodNotAllowed, map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Allow":                       "GET, HEAD, POST, OPTIONS",
	}, fmt.Sprintf("[ERROR] Unsupported method: %s.\n", method), false)
}

func writeText(w http.ResponseWriter, status int, headers map[string]string, body string, head bool) {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Length"] = strconv.Itoa(len([]byte(body)))
	writeHeaders(w, status, headers)
	if head {
		return
	}
	_, _ = io.WriteString(w, body)
}

func writeHeaders(w http.ResponseWriter, status int, headers map[string]string) {
	target := w.Header()
	for key, value := range headers {
		target.Set(key, value)
	}
	w.WriteHeader(status)
}

func writeTransferHeaders(w http.ResponseWriter, senderHeader http.Header) {
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", "*")
	headers.Set("X-Robots-Tag", "none")

	if contentLength := senderHeader.Get("Content-Length"); contentLength != "" {
		headers.Set("Content-Length", contentLength)
	}
	if contentType := sanitizeContentType(senderHeader.Get("Content-Type")); contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if contentDisposition := senderHeader.Get("Content-Disposition"); contentDisposition != "" {
		headers.Set("Content-Disposition", contentDisposition)
	}
	if values, ok := senderHeader["X-Piping"]; ok && len(values) > 0 {
		headers["X-Piping"] = append([]string(nil), values...)
		headers.Set("Access-Control-Expose-Headers", "X-Piping")
	}

	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func sanitizeContentType(contentType string) string {
	if contentType == "" {
		return ""
	}

	lower := strings.ToLower(contentType)
	if strings.HasPrefix(lower, "text/html") {
		return "text/plain" + contentType[len("text/html"):]
	}
	return contentType
}

func writeChunk(w http.ResponseWriter, chunk []byte) bool {
	written := 0
	for written < len(chunk) {
		n, err := w.Write(chunk[written:])
		if err != nil {
			return false
		}
		if n <= 0 {
			return false
		}
		written += n
	}

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func senderBodyClose(req *http.Request) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
}
