package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fastHeaderLimit = 1 << 20
	fastBodyLimit   = 256 << 20
)

var (
	fastWriters    = sync.Pool{New: func() any { return new(fastResponseWriter) }}
	fastBuffers    = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	fastBadRequest = errors.New("bad HTTP request")
	fastDate       struct {
		sync.Mutex
		second int64
		value  string
	}
)

// fastResponseWriter deliberately implements neither Flusher nor Hijacker.
type fastResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *fastResponseWriter) Header() http.Header { return w.header }

func (w *fastResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *fastResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func serveFast(l net.Listener, app *App) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				continue
			}
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go fastConnection(conn, app)
	}
}

// fastLine bounds allocation even when a peer never terminates a header line.
func fastLine(r *bufio.Reader, remaining *int) (string, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > *remaining {
			return "", fastBadRequest
		}
		*remaining -= len(part)
		if err == bufio.ErrBufferFull {
			line = append(line, part...)
			continue
		}
		if err != nil {
			return "", err
		}
		if len(part) < 2 || part[len(part)-2] != '\r' {
			// CR can fall at the end of the previous buffer fragment.
			if len(part) == 1 && len(line) > 0 && line[len(line)-1] == '\r' {
				return string(line[:len(line)-1]), nil
			}
			return "", fastBadRequest
		}
		part = part[:len(part)-2]
		if line != nil {
			part = append(line, part...)
		}
		return string(part), nil
	}
}

func fastToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func fastHeader(line string) (string, string, error) {
	key, value, ok := strings.Cut(line, ":")
	if !ok || !fastToken(key) {
		return "", "", fastBadRequest
	}
	for i := 0; i < len(value); i++ {
		if value[i] == 127 || value[i] < 32 && value[i] != '\t' {
			return "", "", fastBadRequest
		}
	}
	return textproto.CanonicalMIMEHeaderKey(key), strings.Trim(value, " \t"), nil
}

func fastRequest(r *bufio.Reader, conn net.Conn) (*http.Request, bool, error) {
	remaining := fastHeaderLimit
	line, err := fastLine(r, &remaining)
	if err != nil {
		return nil, true, err
	}
	method, rest, ok := strings.Cut(line, " ")
	target, proto, versionOK := strings.Cut(rest, " ")
	if !ok || !versionOK || !fastToken(method) || target == "" || (proto != "HTTP/1.1" && proto != "HTTP/1.0") {
		return nil, true, fastBadRequest
	}
	u, err := url.ParseRequestURI(target)
	if err != nil {
		return nil, true, fastBadRequest
	}
	header := make(http.Header)
	var host string
	haveHost, haveLength, chunked, closeToken, keepToken := false, false, false, false, false
	var length int64
	for {
		line, err = fastLine(r, &remaining)
		if err != nil {
			return nil, true, err
		}
		if line == "" {
			break
		}
		key, value, err := fastHeader(line)
		if err != nil {
			return nil, true, err
		}
		header[key] = append(header[key], value)
		switch key {
		case "Host":
			if !haveHost {
				host, haveHost = value, true
			}
		case "Content-Length":
			if value == "" {
				return nil, true, fastBadRequest
			}
			for i := 0; i < len(value); i++ {
				if value[i] < '0' || value[i] > '9' {
					return nil, true, fastBadRequest
				}
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n > fastBodyLimit || haveLength && n != length {
				return nil, true, fastBadRequest
			}
			length, haveLength = n, true
		case "Transfer-Encoding":
			for _, token := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(token), "chunked") {
					chunked = true
				}
			}
		case "Connection":
			for _, token := range strings.Split(strings.ToLower(value), ",") {
				switch strings.TrimSpace(token) {
				case "close":
					closeToken = true
				case "keep-alive":
					keepToken = true
				}
			}
		}
	}
	for _, expect := range header.Values("Expect") {
		if strings.EqualFold(expect, "100-continue") {
			_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if err := fastWrite(conn, []byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return nil, true, err
			}
			break
		}
	}
	var body []byte
	if chunked {
		body, err = fastChunked(r)
		length = -1
	} else if haveLength {
		body = make([]byte, int(length))
		_, err = io.ReadFull(r, body)
	}
	if err != nil {
		return nil, true, err
	}
	minor := 1
	if proto == "HTTP/1.0" {
		minor = 0
	}
	closeConn := closeToken || minor == 0 && !keepToken
	req := &http.Request{
		Method: method, RequestURI: target, URL: u,
		Proto: proto, ProtoMajor: 1, ProtoMinor: minor,
		Header: header, Host: host, ContentLength: length,
		Body: io.NopCloser(bytes.NewReader(body)), RemoteAddr: conn.RemoteAddr().String(),
		Close: closeConn,
	}
	if chunked {
		req.TransferEncoding = []string{"chunked"}
	}
	return req, closeConn, nil
}

func fastChunked(r *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		remaining := fastHeaderLimit
		line, err := fastLine(r, &remaining)
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(line, ";")
		if size == "" {
			return nil, fastBadRequest
		}
		for i := 0; i < len(size); i++ {
			c := size[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return nil, fastBadRequest
			}
		}
		n, err := strconv.ParseUint(size, 16, 64)
		if err != nil || n > uint64(fastBodyLimit-len(body)) {
			return nil, fastBadRequest
		}
		if n == 0 {
			remaining = fastHeaderLimit
			for {
				line, err = fastLine(r, &remaining)
				if err != nil {
					return nil, err
				}
				if line == "" {
					return body, nil
				}
				if _, _, err = fastHeader(line); err != nil {
					return nil, err
				}
			}
		}
		start := len(body)
		bodyEnd := start + int(n)
		if bodyEnd > cap(body) {
			capacity := cap(body) * 2
			if capacity < bodyEnd {
				capacity = bodyEnd
			}
			if capacity > fastBodyLimit {
				capacity = fastBodyLimit
			}
			grown := make([]byte, bodyEnd, capacity)
			copy(grown, body)
			body = grown
		} else {
			body = body[:bodyEnd]
		}
		if _, err = io.ReadFull(r, body[start:bodyEnd]); err != nil {
			return nil, err
		}
		var end [2]byte
		if _, err = io.ReadFull(r, end[:]); err != nil {
			return nil, err
		}
		if end != [2]byte{'\r', '\n'} {
			return nil, fastBadRequest
		}
	}
}

func fastDispatch(app *App, w *fastResponseWriter, req *http.Request) (panicked bool) {
	panicked = true
	defer func() {
		if panicked {
			_ = recover()
			if w.status == 0 {
				w.status = http.StatusInternalServerError
				w.header.Set("Content-Type", "application/json")
				w.body.Reset()
				w.body.WriteString(`{"message":"Internal Server Error"}`)
			}
		}
	}()
	app.ServeHTTP(w, req)
	return false
}

func fastDateValue() string {
	now := time.Now()
	fastDate.Lock()
	defer fastDate.Unlock()
	if fastDate.value == "" || fastDate.second != now.Unix() {
		fastDate.second = now.Unix()
		fastDate.value = now.UTC().Format(http.TimeFormat)
	}
	return fastDate.value
}

func fastWrite(conn net.Conn, p []byte) error {
	for len(p) > 0 {
		n, err := conn.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func fastConnection(conn net.Conn, app *App) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 32<<10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		if _, err := r.Peek(1); err != nil {
			return
		}
		req, closeConn, err := fastRequest(r, conn)
		if err != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			_ = fastWrite(conn, []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			return
		}
		w := fastWriters.Get().(*fastResponseWriter)
		w.header = make(http.Header)
		w.status = 0
		w.body.Reset()
		closeConn = fastDispatch(app, w, req) || closeConn
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if w.status == http.StatusNoContent || w.status == http.StatusNotModified {
			w.body.Reset()
		}
		out := fastBuffers.Get().(*bytes.Buffer)
		out.Reset()
		out.WriteString(req.Proto)
		out.WriteByte(' ')
		out.WriteString(strconv.Itoa(w.status))
		out.WriteByte(' ')
		statusText := http.StatusText(w.status)
		if statusText == "" {
			statusText = "Status"
		}
		out.WriteString(statusText)
		out.WriteString("\r\n")
		for key, values := range w.header {
			if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Date") {
				continue
			}
			for _, value := range values {
				out.WriteString(key)
				out.WriteString(": ")
				out.WriteString(value)
				out.WriteString("\r\n")
			}
		}
		out.WriteString("Content-Length: ")
		out.WriteString(strconv.Itoa(w.body.Len()))
		out.WriteString("\r\nDate: ")
		out.WriteString(fastDateValue())
		out.WriteString("\r\n")
		if closeConn {
			out.WriteString("Connection: close\r\n")
		} else if req.ProtoMinor == 0 {
			out.WriteString("Connection: keep-alive\r\n")
		}
		out.WriteString("\r\n")
		if req.Method != "HEAD" {
			out.Write(w.body.Bytes())
		}
		_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
		err = fastWrite(conn, out.Bytes())
		w.header = nil
		// Avoid retaining unusually large responses in the pools.
		if w.body.Cap() <= 1<<20 {
			fastWriters.Put(w)
		}
		if out.Cap() <= 1<<20 {
			fastBuffers.Put(out)
		}
		if err != nil || closeConn {
			return
		}
	}
}
