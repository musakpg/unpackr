package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	bufferPoolsMu               sync.Mutex
	bufferPools                 = make(map[int]*sync.Pool)
	globalSpeedLimitBytesPerSec atomic.Int64
)

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	lastUpdate time.Time
}

var globalRateLimiter = &tokenBucket{}

// waitN blocks until n bytes are permitted under the active atomic bandwidth cap.
func (tb *tokenBucket) waitN(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		limit := globalSpeedLimitBytesPerSec.Load()
		if limit <= 0 {
			return nil
		}

		tb.mu.Lock()
		now := time.Now()
		if tb.lastUpdate.IsZero() {
			tb.lastUpdate = now
			tb.tokens = float64(limit) * 0.25
		}
		elapsed := now.Sub(tb.lastUpdate).Seconds()
		tb.lastUpdate = now
		tb.tokens += elapsed * float64(limit)

		maxBurst := float64(limit) * 0.25
		if maxBurst < 256*1024 {
			maxBurst = 256 * 1024
		}
		if tb.tokens > maxBurst {
			tb.tokens = maxBurst
		}

		if tb.tokens >= float64(n) {
			tb.tokens -= float64(n)
			tb.mu.Unlock()
			return nil
		}

		deficit := float64(n) - tb.tokens
		waitDur := time.Duration((deficit / float64(limit)) * float64(time.Second))
		if waitDur > 100*time.Millisecond {
			waitDur = 100 * time.Millisecond
		}
		if waitDur < 5*time.Millisecond {
			waitDur = 5 * time.Millisecond
		}
		tb.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDur):
		}
	}
}

type rateLimitedReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
}

func (r *rateLimitedReadCloser) Read(p []byte) (int, error) {
	limit := globalSpeedLimitBytesPerSec.Load()
	if limit <= 0 {
		return r.rc.Read(p)
	}

	// Cap read slice size when throttling so telemetry updates stay smooth.
	maxChunk := 128 * 1024
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}

	n, err := r.rc.Read(p)
	if n > 0 {
		if waitErr := globalRateLimiter.waitN(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

func (r *rateLimitedReadCloser) Close() error {
	return r.rc.Close()
}

type throttledTransport struct {
	base http.RoundTripper
}

func (t *throttledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &rateLimitedReadCloser{
		ctx: req.Context(),
		rc:  resp.Body,
	}
	return resp, nil
}

func acquireBuffer(sizeMB int) []byte {
	switch sizeMB {
	case 1, 2, 4, 8, 16:
	default:
		sizeMB = defaultBufferMB
	}

	bufferPoolsMu.Lock()
	pool, exists := bufferPools[sizeMB]
	if !exists {
		byteLen := sizeMB * 1024 * 1024
		pool = &sync.Pool{
			New: func() interface{} {
				return make([]byte, byteLen)
			},
		}
		bufferPools[sizeMB] = pool
	}
	bufferPoolsMu.Unlock()

	return pool.Get().([]byte)
}

func releaseBuffer(sizeMB int, buf []byte) {
	switch sizeMB {
	case 1, 2, 4, 8, 16:
	default:
		sizeMB = defaultBufferMB
	}

	bufferPoolsMu.Lock()
	pool, exists := bufferPools[sizeMB]
	bufferPoolsMu.Unlock()

	if exists && len(buf) == sizeMB*1024*1024 {
		pool.Put(buf)
	}
}

func createOptimizedHTTPClient(timeout time.Duration) *http.Client {
	return buildHTTPClient(timeout, false)
}

func createStreamingHTTPClient(timeout time.Duration) *http.Client {
	return buildHTTPClient(timeout, true)
}

func buildHTTPClient(timeout time.Duration, throttled bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   20 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	var transport http.RoundTripper = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		WriteBufferSize:       256 * 1024,
		ReadBufferSize:        512 * 1024,
	}
	if throttled {
		transport = &throttledTransport{base: transport}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func applyCustomHeaders(req *http.Request, headers map[string]string) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	for k, v := range headers {
		tk := strings.TrimSpace(k)
		tv := strings.TrimSpace(v)
		if tk != "" && tv != "" {
			req.Header.Set(tk, tv)
		}
	}
}

// probeRemoteURL probes a remote URL using HEAD, falling back to GET Range: bytes=0-0 on 405 Method Not Allowed or 403 Forbidden.
func probeRemoteURL(ctx context.Context, client *http.Client, targetURL string, headers map[string]string) (*http.Response, string, int64, error) {
	headReq, err := http.NewRequestWithContext(ctx, "HEAD", targetURL, nil)
	if err != nil {
		return nil, targetURL, 0, err
	}
	applyCustomHeaders(headReq, headers)

	resp, err := client.Do(headReq)
	if err == nil {
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			resolved := targetURL
			if resp.Request != nil && resp.Request.URL != nil {
				resolved = resp.Request.URL.String()
			}
			return resp, resolved, resp.ContentLength, nil
		}
		// On 405 Method Not Allowed, 403 Forbidden, or other refusal of HEAD, cleanly fall back to GET Range: bytes=0-0
		if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 400 {
			_ = resp.Body.Close()
		} else {
			_ = resp.Body.Close()
			return nil, targetURL, 0, fmt.Errorf("archive server returned HTTP %s", resp.Status)
		}
	}

	getReq, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, targetURL, 0, err
	}
	applyCustomHeaders(getReq, headers)
	getReq.Header.Set("Range", "bytes=0-0")

	getResp, err := client.Do(getReq)
	if err != nil {
		return nil, targetURL, 0, err
	}

	resolved := targetURL
	if getResp.Request != nil && getResp.Request.URL != nil {
		resolved = getResp.Request.URL.String()
	}

	var totalLen int64 = getResp.ContentLength
	if getResp.StatusCode == http.StatusPartialContent {
		cr := getResp.Header.Get("Content-Range")
		if slashIdx := strings.LastIndex(cr, "/"); slashIdx != -1 {
			totalStr := strings.TrimSpace(cr[slashIdx+1:])
			if parsed, parseErr := strconv.ParseInt(totalStr, 10, 64); parseErr == nil && parsed > 0 {
				totalLen = parsed
			}
		}
	}

	return getResp, resolved, totalLen, nil
}

type ParallelRangeStream struct {
	ctx          context.Context
	client       *http.Client
	url          string
	totalSize    int64
	workers      int
	chunkSize    int64
	readOffset   int64
	bytesFetched atomic.Int64
	currentBuf   []byte
	currentPos   int
	chunks       map[int64][]byte
	mu           sync.Mutex
	cond         *sync.Cond
	nextFetchID  int64
	fetchErr     error
	closed       bool
	headers      map[string]string
	onBytesRead  func(n int64)
}

func NewParallelRangeStream(ctx context.Context, client *http.Client, targetURL string, totalSize int64, workers int, headers map[string]string, onBytesRead func(n int64)) *ParallelRangeStream {
	if workers < 1 {
		workers = 1
	}
	if workers > 8 {
		workers = 8
	}
	prs := &ParallelRangeStream{
		ctx:         ctx,
		client:      client,
		url:         targetURL,
		totalSize:   totalSize,
		workers:     workers,
		chunkSize:   prefetchChunkSz,
		chunks:      make(map[int64][]byte),
		headers:     headers,
		onBytesRead: onBytesRead,
	}
	prs.cond = sync.NewCond(&prs.mu)

	for i := 0; i < workers; i++ {
		go prs.workerLoop()
	}
	return prs
}

func (prs *ParallelRangeStream) workerLoop() {
	totalChunks := (prs.totalSize + prs.chunkSize - 1) / prs.chunkSize

	for {
		prs.mu.Lock()
		for !prs.closed && prs.fetchErr == nil && int64(len(prs.chunks)) >= int64(prs.workers*2) {
			prs.cond.Wait()
		}
		if prs.closed || prs.fetchErr != nil || prs.ctx.Err() != nil {
			prs.mu.Unlock()
			return
		}
		chunkID := prs.nextFetchID
		if chunkID >= totalChunks {
			prs.mu.Unlock()
			return
		}
		prs.nextFetchID++
		prs.mu.Unlock()

		start := chunkID * prs.chunkSize
		end := start + prs.chunkSize - 1
		if end >= prs.totalSize {
			end = prs.totalSize - 1
		}

		data, err := prs.fetchRangeWithRetry(start, end)
		prs.mu.Lock()
		if err != nil {
			if prs.fetchErr == nil {
				prs.fetchErr = err
			}
			prs.cond.Broadcast()
			prs.mu.Unlock()
			return
		}
		prs.chunks[chunkID] = data
		prs.cond.Broadcast()
		prs.mu.Unlock()
	}
}

func (prs *ParallelRangeStream) fetchRangeWithRetry(start, end int64) ([]byte, error) {
	var lastErr error
	attempt := 1
	for attempt <= maxRetries {
		if prs.ctx.Err() != nil {
			return nil, prs.ctx.Err()
		}
		if attempt > 1 {
			backoff := time.Duration(1<<(attempt-2)) * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			select {
			case <-prs.ctx.Done():
				return nil, prs.ctx.Err()
			case <-time.After(backoff + time.Duration(rand.Intn(300))*time.Millisecond):
			}
		}

		prs.mu.Lock()
		targetURL := prs.url
		prs.mu.Unlock()
		hasProgress := prs.bytesFetched.Load() > 0

		req, err := http.NewRequestWithContext(prs.ctx, "GET", targetURL, nil)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}
		applyCustomHeaders(req, prs.headers)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		resp, err := prs.client.Do(req)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}

		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && hasProgress {
			_ = resp.Body.Close()
			newURL, waitErr := manager.WaitForRefreshedURL(prs.ctx, targetURL, resp.StatusCode)
			if waitErr != nil {
				return nil, waitErr
			}
			prs.mu.Lock()
			prs.url = newURL
			prs.mu.Unlock()
			attempt = 1
			continue
		}

		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("unexpected HTTP status %s for range %d-%d", resp.Status, start, end)
			attempt++
			continue
		}

		expectedLen := (end - start) + 1
		buf := make([]byte, expectedLen)
		_, readErr := io.ReadFull(resp.Body, buf)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			attempt++
			continue
		}
		prs.bytesFetched.Add(expectedLen)
		return buf, nil
	}
	return nil, fmt.Errorf("chunk fetch failed after retries: %w", lastErr)
}

func (prs *ParallelRangeStream) Read(p []byte) (int, error) {
	if prs.ctx.Err() != nil {
		return 0, prs.ctx.Err()
	}

	if prs.currentPos >= len(prs.currentBuf) {
		if prs.readOffset >= prs.totalSize {
			return 0, io.EOF
		}

		chunkID := prs.readOffset / prs.chunkSize
		prs.mu.Lock()
		for {
			if prs.closed || prs.ctx.Err() != nil {
				prs.mu.Unlock()
				return 0, context.Canceled
			}
			if data, ok := prs.chunks[chunkID]; ok {
				delete(prs.chunks, chunkID)
				prs.currentBuf = data
				prs.currentPos = 0
				prs.cond.Broadcast()
				break
			}
			if prs.fetchErr != nil {
				err := prs.fetchErr
				prs.mu.Unlock()
				return 0, err
			}
			prs.cond.Wait()
		}
		prs.mu.Unlock()
	}

	n := copy(p, prs.currentBuf[prs.currentPos:])
	prs.currentPos += n
	prs.readOffset += int64(n)
	if prs.onBytesRead != nil && n > 0 {
		prs.onBytesRead(int64(n))
	}
	return n, nil
}

func (prs *ParallelRangeStream) Close() error {
	prs.mu.Lock()
	prs.closed = true
	prs.chunks = nil
	prs.cond.Broadcast()
	prs.mu.Unlock()
	return nil
}

type ResilientSingleStream struct {
	ctx           context.Context
	client        *http.Client
	resolvedURL   string
	currentOffset int64
	totalBytes    int64
	currentBody   io.ReadCloser
	headers       map[string]string
	onBytesRead   func(n int64)
}

func NewResilientSingleStream(ctx context.Context, client *http.Client, resolvedURL string, totalBytes int64, initialBody io.ReadCloser, headers map[string]string, onBytesRead func(n int64)) *ResilientSingleStream {
	manager.RegisterActiveBody(initialBody)
	return &ResilientSingleStream{
		ctx:         ctx,
		client:      client,
		resolvedURL: resolvedURL,
		totalBytes:  totalBytes,
		currentBody: initialBody,
		headers:     headers,
		onBytesRead: onBytesRead,
	}
}

func (rs *ResilientSingleStream) Read(p []byte) (int, error) {
	if rs.ctx.Err() != nil {
		return 0, rs.ctx.Err()
	}
	if rs.currentBody == nil {
		if err := rs.reconnect(); err != nil {
			return 0, err
		}
	}

	n, err := rs.currentBody.Read(p)
	if n > 0 {
		rs.currentOffset += int64(n)
		if rs.onBytesRead != nil {
			rs.onBytesRead(int64(n))
		}
	}

	if err != nil && err != io.EOF {
		if rs.ctx.Err() != nil {
			return n, rs.ctx.Err()
		}
		if recErr := rs.reconnect(); recErr != nil {
			return n, recErr
		}
		return rs.Read(p[n:])
	}
	return n, err
}

func (rs *ResilientSingleStream) reconnect() error {
	if rs.currentBody != nil {
		_ = rs.currentBody.Close()
		rs.currentBody = nil
	}

	var lastErr error
	attempt := 1
	skipBackoff := false
	for attempt <= maxRetries {
		if rs.ctx.Err() != nil {
			return rs.ctx.Err()
		}

		if !skipBackoff {
			backoffSec := 1 << (attempt - 1)
			if backoffSec > 15 {
				backoffSec = 15
			}
			jitter := time.Duration(rand.Intn(500)) * time.Millisecond

			manager.Update(func(s *ExtractionState) {
				s.Message = fmt.Sprintf("Reconnecting stream (attempt %d/%d in %ds)...", attempt, maxRetries, backoffSec)
			})

			select {
			case <-rs.ctx.Done():
				return rs.ctx.Err()
			case <-time.After(time.Duration(backoffSec)*time.Second + jitter):
			}
		}
		skipBackoff = false

		req, err := http.NewRequestWithContext(rs.ctx, "GET", rs.resolvedURL, nil)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}
		applyCustomHeaders(req, rs.headers)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rs.currentOffset))

		resp, err := rs.client.Do(req)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}

		if resp.StatusCode == http.StatusPartialContent {
			rs.currentBody = resp.Body
			manager.RegisterActiveBody(resp.Body)
			if resp.Request != nil && resp.Request.URL != nil {
				rs.resolvedURL = resp.Request.URL.String()
			}
			manager.Update(func(s *ExtractionState) {
				s.Message = "Stream connection restored. Resuming extraction..."
			})
			return nil
		}

		_ = resp.Body.Close()
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && rs.currentOffset > 0 {
			newURL, waitErr := manager.WaitForRefreshedURL(rs.ctx, rs.resolvedURL, resp.StatusCode)
			if waitErr != nil {
				return waitErr
			}
			rs.resolvedURL = newURL
			attempt = 1
			skipBackoff = true
			continue
		}
		if resp.StatusCode == http.StatusOK && rs.currentOffset > 0 {
			return fmt.Errorf("host ignored Range resume header (HTTP 200)")
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return fmt.Errorf("download link expired or range rejected (HTTP %s)", resp.Status)
		}
		lastErr = fmt.Errorf("unexpected HTTP status: %s", resp.Status)
		attempt++
	}
	return fmt.Errorf("network recovery failed after %d retries: %w", maxRetries, lastErr)
}

func (rs *ResilientSingleStream) Close() error {
	if rs.currentBody != nil {
		return rs.currentBody.Close()
	}
	return nil
}

type MultiPartStreamReader struct {
	ctx           context.Context
	client        *http.Client
	urls          []string
	parallelConns int
	headers       map[string]string
	currentIdx    int
	activeReader  io.ReadCloser
	bytesRead     atomic.Int64
	onBytesRead   func(n int64)
}

func NewMultiPartStreamReader(ctx context.Context, client *http.Client, urls []string, parallelConns int, headers map[string]string, onBytesRead func(n int64)) *MultiPartStreamReader {
	return &MultiPartStreamReader{
		ctx:           ctx,
		client:        client,
		urls:          urls,
		parallelConns: parallelConns,
		headers:       headers,
		onBytesRead:   onBytesRead,
	}
}

func (mp *MultiPartStreamReader) openNextPart() error {
	if mp.activeReader != nil {
		_ = mp.activeReader.Close()
		mp.activeReader = nil
	}
	if mp.currentIdx >= len(mp.urls) {
		return io.EOF
	}

	targetURL := mp.urls[mp.currentIdx]
	partNum := mp.currentIdx + 1
	totalParts := len(mp.urls)
	mp.currentIdx++

	manager.Update(func(s *ExtractionState) {
		s.CurrentPart = partNum
		s.TotalParts = totalParts
		if totalParts > 1 {
			s.Message = fmt.Sprintf("Streaming archive part %d of %d...", partNum, totalParts)
		}
	})

	for {
		req, err := http.NewRequestWithContext(mp.ctx, "GET", targetURL, nil)
		if err != nil {
			return err
		}
		applyCustomHeaders(req, mp.headers)

		resp, err := mp.client.Do(req)
		if err != nil {
			return err
		}
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && mp.bytesRead.Load() > 0 {
			_ = resp.Body.Close()
			newURL, waitErr := manager.WaitForRefreshedURL(mp.ctx, targetURL, resp.StatusCode)
			if waitErr != nil {
				return waitErr
			}
			targetURL = newURL
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			return fmt.Errorf("part %d returned HTTP %s", partNum, resp.Status)
		}

		resolvedURL := targetURL
		if resp.Request != nil && resp.Request.URL != nil {
			resolvedURL = resp.Request.URL.String()
		}

		acceptRanges := strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
		if mp.parallelConns > 1 && acceptRanges && resp.ContentLength > int64(prefetchChunkSz*2) {
			_ = resp.Body.Close()
			prs := NewParallelRangeStream(mp.ctx, mp.client, resolvedURL, resp.ContentLength, mp.parallelConns, mp.headers, mp.onBytesRead)
			manager.RegisterActiveBody(prs)
			mp.activeReader = prs
			return nil
		}

		rss := NewResilientSingleStream(mp.ctx, mp.client, resolvedURL, resp.ContentLength, resp.Body, mp.headers, mp.onBytesRead)
		mp.activeReader = rss
		return nil
	}
}

func (mp *MultiPartStreamReader) Read(p []byte) (int, error) {
	for {
		if mp.ctx.Err() != nil {
			return 0, mp.ctx.Err()
		}
		if mp.activeReader == nil {
			if err := mp.openNextPart(); err != nil {
				return 0, err
			}
		}
		n, err := mp.activeReader.Read(p)
		if n > 0 {
			mp.bytesRead.Add(int64(n))
			return n, nil
		}
		if err == io.EOF {
			_ = mp.activeReader.Close()
			mp.activeReader = nil
			continue
		}
		return 0, err
	}
}

func (mp *MultiPartStreamReader) Close() error {
	if mp.activeReader != nil {
		return mp.activeReader.Close()
	}
	return nil
}

