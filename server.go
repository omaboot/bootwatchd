package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// limits are what one address may ask for, and what the server serves in a day in all. Behind
// a NAT a whole room shares an address (a repair shop, a meetup), so the per-address limits
// leave room for a cart of Macs; the daily cap protects a small droplet's transfer.
type limits struct {
	echoPerMin    int   // echoes a minute
	streamPerHour int64 // stream bytes an hour
	sinkPerHour   int64 // sink bytes an hour
	dailyBytes    int64 // stream and sink bytes a day (UTC), all addresses together
}

// defaultLimits: about 20 full runs an hour from one address, and 4 GiB a day in all.
var defaultLimits = limits{echoPerMin: 600, streamPerHour: 64 << 20, sinkPerHour: 16 << 20, dailyBytes: 4 << 30}

// server is the handler. It keeps no request log and no addresses on disk: the limits are kept
// in memory per address and forgotten after an hour, and the counters (healthPath) are totals
// only. On disk there's only today's byte count (dayFile), so a restart doesn't reset the
// daily cap.
type server struct {
	lim limits
	// cloudflare: behind Cloudflare's proxy, so a request's address is its CF-Connecting-IP
	// header. Only with a firewall that lets nothing but Cloudflare in.
	cloudflare bool
	now        func() time.Time

	mu       sync.Mutex
	addrs    map[string]*usage
	purged   time.Time
	day      string // the UTC day dayBytes counts
	dayBytes int64
	dayFrom  time.Time // when dayBytes started counting: the day's start, or this server's
	saved    []byte    // the day file as last written
	started  time.Time
	h        health
}

type usage struct {
	minute, hour time.Time // the windows' starts
	echoes       int
	stream, sink int64
}

// health is healthPath's answer: totals since the server started, nothing per address.
type health struct {
	Version     string    `json:"version"` // the build, from git describe (a release tag)
	Since       time.Time `json:"since"`
	Echoes      int64     `json:"echoes"`
	Streams     int64     `json:"streams"`
	StreamBytes int64     `json:"stream_bytes"`
	Sinks       int64     `json:"sinks"`
	SinkBytes   int64     `json:"sink_bytes"`
	SinkWrong   int64     `json:"sink_wrong"` // uploads that arrived with a wrong byte
	Limited     int64     `json:"limited"`    // requests turned away by a limit
	Bad         int64     `json:"bad"`        // malformed requests
	DayBytes    int64     `json:"day_bytes"`  // stream and sink bytes today (UTC)
	DailyBytes  int64     `json:"daily_bytes"`
	// DayBytesSince is when day_bytes started counting: midnight UTC, or later when the server
	// started today without a saved count of today's.
	DayBytesSince time.Time `json:"day_bytes_since"`
}

// sinkAnswer is the sink's answer.
type sinkAnswer struct {
	N     int64 `json:"n"`     // bytes read
	Match int64 `json:"match"` // leading bytes equal to the seed's stream
}

func newServer(l limits, cloudflare bool) *server {
	now := time.Now
	t := now()
	return &server{lim: l, cloudflare: cloudflare, now: now, addrs: map[string]*usage{}, started: t, purged: t,
		day: t.UTC().Format(time.DateOnly), dayFrom: t.UTC().Truncate(time.Second)}
}

// dayCount is the day file: today's byte count, and nothing about anyone.
type dayCount struct {
	Day   string    `json:"day"` // UTC
	Bytes int64     `json:"bytes"`
	From  time.Time `json:"from"`
}

// loadDay takes up the byte count saved at path if it's today's. A missing file is a first start.
func (s *server) loadDay(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var c dayCount
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDay(s.now())
	if c.Day == s.day {
		s.dayBytes, s.dayFrom = c.Bytes, c.From
	}
	s.saved = b
	return nil
}

// saveDay writes the day's byte count to path when it changed: a new file renamed over the old,
// so a crash leaves one or the other whole.
func (s *server) saveDay(path string) error {
	s.mu.Lock()
	s.rollDay(s.now())
	b, _ := json.Marshal(dayCount{Day: s.day, Bytes: s.dayBytes, From: s.dayFrom})
	b = append(b, '\n')
	same := bytes.Equal(b, s.saved)
	s.mu.Unlock()
	if same {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".day-*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	s.mu.Lock()
	s.saved = b
	s.mu.Unlock()
	return nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-transform")
	switch r.URL.Path {
	case echoPath:
		s.echo(w, r)
	case streamPath:
		s.stream(w, r)
	case sinkPath:
		s.sink(w, r)
	case healthPath:
		s.health(w)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) echo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.bad(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	n, err := parseSeed(r.URL.Query().Get("n"))
	if err != nil {
		s.bad(w, http.StatusBadRequest, "n: "+err.Error())
		return
	}
	if !s.take(r, func(u *usage, now time.Time) bool {
		if now.Sub(u.minute) >= time.Minute {
			u.minute, u.echoes = now, 0
		}
		if u.echoes >= s.lim.echoPerMin {
			return false
		}
		u.echoes++
		s.h.Echoes++
		return true
	}) {
		s.busy(w, time.Minute)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, n.String())
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.bad(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	sd, err := parseSeed(q.Get("seed"))
	if err != nil {
		s.bad(w, http.StatusBadRequest, "seed: "+err.Error())
		return
	}
	n, err := strconv.ParseInt(q.Get("n"), 10, 64)
	if err != nil || n < 1 || n > maxStream {
		s.bad(w, http.StatusBadRequest, fmt.Sprintf("n: 1 to %d bytes", maxStream))
		return
	}
	if !s.take(r, func(u *usage, _ time.Time) bool {
		if !s.charge(&u.stream, s.lim.streamPerHour, n) {
			return false
		}
		s.h.Streams++
		s.h.StreamBytes += n
		return true
	}) {
		s.busy(w, time.Hour)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	io.CopyN(w, stream(sd), n)
}

func (s *server) sink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.bad(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	sd, err := parseSeed(r.URL.Query().Get("seed"))
	if err != nil {
		s.bad(w, http.StatusBadRequest, "seed: "+err.Error())
		return
	}
	n := r.ContentLength
	switch {
	case n < 0:
		s.bad(w, http.StatusLengthRequired, "a Content-Length")
		return
	case n > maxSink:
		s.bad(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d bytes", maxSink))
		return
	}
	if !s.take(r, func(u *usage, _ time.Time) bool { return s.charge(&u.sink, s.lim.sinkPerHour, n) }) {
		s.busy(w, time.Hour)
		return
	}
	got, ok, err := match(http.MaxBytesReader(w, r.Body, maxSink), sd)
	if err != nil {
		s.bad(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	s.mu.Lock()
	s.h.Sinks++
	s.h.SinkBytes += got
	if ok != got {
		s.h.SinkWrong++
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sinkAnswer{N: got, Match: ok})
}

func (s *server) health(w http.ResponseWriter) {
	s.mu.Lock()
	s.rollDay(s.now())
	h := s.h
	h.Version, h.Since, h.DayBytes, h.DailyBytes = version, s.started.UTC().Truncate(time.Second), s.dayBytes, s.lim.dailyBytes
	h.DayBytesSince = s.dayFrom
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h)
}

// take runs ok on the request's address's usage, under the lock; false turns the request away.
func (s *server) take(r *http.Request, ok func(u *usage, now time.Time) bool) bool {
	a := s.addr(r)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.purged) >= 10*time.Minute { // forget addresses idle for an hour
		for k, u := range s.addrs {
			if now.Sub(u.hour) >= time.Hour && now.Sub(u.minute) >= time.Minute {
				delete(s.addrs, k)
			}
		}
		s.purged = now
	}
	s.rollDay(now)
	u := s.addrs[a]
	if u == nil {
		u = &usage{minute: now, hour: now}
		s.addrs[a] = u
	}
	if now.Sub(u.hour) >= time.Hour {
		u.hour, u.stream, u.sink = now, 0, 0
	}
	if !ok(u, now) {
		s.h.Limited++
		return false
	}
	return true
}

// charge adds n bytes to an address's hourly count and the day's, if both have room. Under the
// lock.
func (s *server) charge(used *int64, limit, n int64) bool {
	if *used+n > limit || s.dayBytes+n > s.lim.dailyBytes {
		return false
	}
	*used += n
	s.dayBytes += n
	return true
}

func (s *server) rollDay(now time.Time) {
	if d := now.UTC().Format(time.DateOnly); d != s.day {
		s.day, s.dayBytes = d, 0
		s.dayFrom, _ = time.Parse(time.DateOnly, d)
	}
}

// addr is who's asking, for the limits: the client's address, or with Cloudflare in front its
// CF-Connecting-IP; an IPv6 address counts by its /64 (one household, or one host).
func (s *server) addr(r *http.Request) string {
	a := ""
	if s.cloudflare {
		a = r.Header.Get("CF-Connecting-IP")
	}
	if a == "" {
		a, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	ip := net.ParseIP(a)
	switch {
	case ip == nil:
		return a
	case ip.To4() == nil:
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

func (s *server) bad(w http.ResponseWriter, code int, what string) {
	s.mu.Lock()
	s.h.Bad++
	s.mu.Unlock()
	http.Error(w, "bootwatchd: "+what, code)
}

func (s *server) busy(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	http.Error(w, "bootwatchd: busy, try later", http.StatusTooManyRequests)
}
