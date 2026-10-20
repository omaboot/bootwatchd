package main

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The stream is the protocol: README.md's test vectors (OpenSSL's aes-128-ctr gives the same),
// and, block by block, AES-128 of a counter that counts up as one big-endian 128-bit number.
func TestStreamVectors(t *testing.T) {
	for _, c := range []struct{ seed, want string }{
		{"00000000000000000000000000000000", "66e94bd4ef8a2c3b884cfa59ca342b2e58e2fccefa7e3061367f1d57a4e7455a"},
		{"000102030405060708090a0b0c0d0e0f", "c6a13b37878f5b826f4f8162a1c8d8797346139595c0b41e497bbde365f42d0a"},
	} {
		sd, _ := parseSeed(c.seed)
		got := make([]byte, 32)
		stream(sd).Read(got)
		if h := hex.EncodeToString(got); h != c.want {
			t.Errorf("seed %s: %s", c.seed, h)
		}
	}
	sd, _ := parseSeed("000102030405060708090a0b0c0d0e0f")
	got := make([]byte, 64)
	stream(sd).Read(got)
	b, _ := aes.NewCipher(sd[:])
	want := make([]byte, 64)
	for i := range 4 {
		var ctr [16]byte
		ctr[15] = byte(i)
		b.Encrypt(want[i*16:], ctr[:])
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream %x, want %x", got, want)
	}
}

func TestMatch(t *testing.T) {
	sd, _ := parseSeed("00112233445566778899aabbccddeeff")
	data := make([]byte, 100_000)
	stream(sd).Read(data)
	bad := bytes.Clone(data)
	bad[70_000] ^= 0x40
	for _, c := range []struct {
		name      string
		body      []byte
		n, wantOK int64
	}{
		{"whole and right", data, 100_000, 100_000},
		{"short", data[:70_000], 70_000, 70_000},
		{"one wrong byte", bad, 100_000, 70_000},
		{"a sign-in page", []byte("<html>please sign in</html>"), 27, 0},
	} {
		n, ok, err := match(bytes.NewReader(c.body), sd)
		if err != nil || n != c.n || ok != c.wantOK {
			t.Errorf("%s: n %d ok %d err %v", c.name, n, ok, err)
		}
	}
}

func TestEndpoints(t *testing.T) {
	srv := httptest.NewServer(newServer(defaultLimits, false))
	defer srv.Close()
	sd := "000102030405060708090a0b0c0d0e0f"

	res, _ := http.Get(srv.URL + echoPath + "?n=" + sd)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != sd || res.Header.Get("Cache-Control") != "no-store, no-transform" {
		t.Fatalf("echo: %d %q %q", res.StatusCode, b, res.Header.Get("Cache-Control"))
	}

	res, _ = http.Get(srv.URL + streamPath + "?seed=" + sd + "&n=100000")
	s, _ := parseSeed(sd)
	n, ok, _ := match(res.Body, s)
	if res.StatusCode != 200 || n != 100000 || ok != n {
		t.Fatalf("stream: %d, %d bytes, %d right", res.StatusCode, n, ok)
	}

	body := make([]byte, 65536)
	stream(s).Read(body)
	body[4096] ^= 1
	res, _ = http.Post(srv.URL+sinkPath+"?seed="+sd, "application/octet-stream", bytes.NewReader(body))
	var a sinkAnswer
	json.NewDecoder(res.Body).Decode(&a)
	if res.StatusCode != 200 || a != (sinkAnswer{N: 65536, Match: 4096}) {
		t.Fatalf("sink: %d %+v", res.StatusCode, a)
	}

	for _, c := range []struct {
		method, path string
		body         io.Reader
		code         int
	}{
		{"GET", streamPath + "?seed=" + sd + "&n=1048577", nil, 400}, // over 1 MiB
		{"GET", streamPath + "?seed=xyz&n=10", nil, 400},
		{"GET", echoPath + "?n=" + sd[:30], nil, 400},
		{"POST", sinkPath + "?seed=" + sd, bytes.NewReader(make([]byte, maxSink+1)), 413},
		{"POST", sinkPath + "?seed=" + sd, io.MultiReader(strings.NewReader("chunked")), 411}, // no length
		{"GET", sinkPath + "?seed=" + sd, nil, 405},
		{"GET", "/", nil, 404},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, c.body)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != c.code {
			t.Errorf("%s %s: %v %v, want %d", c.method, c.path, res.StatusCode, err, c.code)
		}
	}
}

// Behind a NAT or one IPv6 /64, every client counts against the same limits; a stream past the
// hour's bytes or the day's is turned away.
func TestLimits(t *testing.T) {
	s := newServer(limits{echoPerMin: 2, streamPerHour: 300 << 10, sinkPerHour: 1 << 20, dailyBytes: 600 << 10}, false)
	ask := func(addr, path string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	echo := echoPath + "?n=000102030405060708090a0b0c0d0e0f"
	got := []int{ask("[2001:db8::1]:5000", echo), ask("[2001:db8::2]:5000", echo), ask("[2001:db8::3]:5000", echo),
		ask("[2001:db8:0:1::1]:5000", echo), ask("192.0.2.1:5000", echo)}
	if want := []int{200, 200, 429, 200, 200}; !slices.Equal(got, want) {
		t.Fatalf("echoes %v, want %v", got, want)
	}
	part := streamPath + "?seed=000102030405060708090a0b0c0d0e0f&n=262144"
	got = []int{ask("192.0.2.1:5000", part), ask("192.0.2.1:5000", part), ask("192.0.2.2:5000", part), ask("192.0.2.3:5000", part)}
	if want := []int{200, 429, 200, 429}; !slices.Equal(got, want) { // the hour's 300 KB per address, then the day's 600 KB
		t.Fatalf("streams %v, want %v", got, want)
	}
}

// Today's byte count outlives a restart, so restarting can't reset the daily cap; yesterday's
// doesn't carry over. The file holds the count and nothing about anyone.
func TestDayKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "day")
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	start := func() *server {
		s := newServer(limits{echoPerMin: 10, streamPerHour: 1 << 20, sinkPerHour: 1 << 20, dailyBytes: 300 << 10}, false)
		// as newServer starts it at that time: counting from its start until a saved count says more
		s.now, s.started, s.day, s.dayFrom = func() time.Time { return at }, at, at.Format(time.DateOnly), at
		if err := s.loadDay(path); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ask := func(s *server) int {
		r := httptest.NewRequest(http.MethodGet, streamPath+"?seed=000102030405060708090a0b0c0d0e0f&n=131072", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	health := func(s *server) health {
		w := httptest.NewRecorder()
		s.health(w)
		var h health
		json.NewDecoder(w.Body).Decode(&h)
		return h
	}
	s := start()
	if h := health(s); !h.DayBytesSince.Equal(at) {
		t.Fatalf("a first start counts from %v", h.DayBytesSince)
	}
	ask(s)
	ask(s)
	if err := s.saveDay(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"day":"2026-10-17","bytes":262144,"from":"2026-10-17T09:00:00Z"}`+"\n" {
		t.Fatalf("day file %s", b)
	}
	at = at.Add(time.Hour) // restarted: the 256 KiB already served still count
	s = start()
	if code := ask(s); code != http.StatusTooManyRequests {
		t.Fatalf("after a restart: %d, want 429", code)
	}
	if h := health(s); h.DayBytes != 262144 || !h.DayBytesSince.Equal(at.Add(-time.Hour)) {
		t.Fatalf("after a restart: %d bytes since %v", h.DayBytes, h.DayBytesSince)
	}
	at = time.Date(2026, 10, 18, 0, 5, 0, 0, time.UTC) // a new day: from midnight, from zero
	s = start()
	if h := health(s); h.DayBytes != 0 || ask(s) != http.StatusOK {
		t.Fatalf("next day: %d bytes", h.DayBytes)
	}
	s.saveDay(path)
	at = at.Add(time.Minute)
	if h := health(start()); h.DayBytes != 131072 || !h.DayBytesSince.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Add(5*time.Minute)) {
		t.Fatalf("next day after a restart: %d bytes since %v", h.DayBytes, h.DayBytesSince)
	}
}

// A bug report is kept only with a key and the known files, scrubbed again, under its code; the
// limits turn the next one away.
func TestBugreport(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("k", 40)
	s := newServer(defaultLimits, false)
	s.bugs = newBugs(map[string]string{key: "dev"}, dirStore{dir: dir}, bugLimits{perAddrHour: 10, perMacDay: 1, perDay: 10})
	mac := strings.Repeat("ab", 32)
	tarOf := func(files map[string]string) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for n, body := range files {
			tw.WriteHeader(&tar.Header{Name: "bugreport-x/" + n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		return b.Bytes()
	}
	send := func(body []byte, auth, macID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, bugPath, bytes.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		r.Header.Set("X-OmaBoot-Mac", macID)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	good := tarOf(map[string]string{"manifest.json": `{"version":"0.9.3 dev"}`, "kernel-log.txt": "wlan0: authenticate with 3c:22:fb:01:02:03\n"})
	if w := send(good, "", mac); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	if w := send(tarOf(map[string]string{"manifest.json": "{}", "secrets.txt": "x"}), key, mac); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "secrets.txt") {
		t.Fatalf("unknown file: %d %s", w.Code, w.Body)
	}
	w := send(good, key, mac)
	var a bugAnswer
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != http.StatusCreated || !regexp.MustCompile(`^B-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`).MatchString(a.Code) {
		t.Fatalf("kept: %d %s", w.Code, w.Body)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "v1", a.Code+".tar"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(stored))
	names := map[string]string{}
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		b, _ := io.ReadAll(tr)
		names[h.Name] = string(b)
	}
	if kl := names[a.Code+"/kernel-log.txt"]; strings.Contains(kl, "3c:22:fb") || !strings.Contains(kl, "[redacted]") {
		t.Errorf("not scrubbed again: %q", kl)
	}
	if rc := names[a.Code+"/received.json"]; !strings.Contains(rc, `"key": "dev"`) || strings.Contains(rc, "192.0.2") {
		t.Errorf("received.json: %q", rc)
	}
	if w := send(good, key, mac); w.Code != http.StatusTooManyRequests {
		t.Errorf("second from the same Mac today: %d", w.Code)
	}
}
