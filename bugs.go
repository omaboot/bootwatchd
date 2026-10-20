package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/doesitomarchy/doesitomarchy/pkg/scrub"
)

// Bug reports (OmaBoot? PF-13): OmaBoot? Live sends a bug report (its logs, its report and its
// screen as text, already scrubbed on the Mac) with an upload key from its image. bootwatchd
// checks the key and the limits, scrubs every file again with the same rules
// (DoesItOmarchy's pkg/scrub), and passes it on to private storage: an R2 bucket in production,
// a directory in development. It keeps nothing of it, and nothing about the sender: the limits
// are in memory, per address and per Mac (a hash the Mac sends), and forgotten within a day.
const bugPath = "/v1/bugreport" // POST + tar body, Authorization: Bearer <upload key>, X-OmaBoot-Mac: <64 hex>

const (
	maxBug      = 1 << 20 // a bug report as sent (about 150 KB today)
	maxBugFiles = 4 << 20 // its files, unpacked
	bugsAtOnce  = 2       // uploads being scrubbed and stored at a time, so a small droplet stays responsive
)

// bugFiles are the files a bug report may hold: what OmaBoot? Live's bugreport writes, and
// its manifest. Anything else is refused, so nothing unexpected is ever stored.
var bugFiles = map[string]bool{"manifest.json": true, "full-log.txt": true, "kernel-log.txt": true, "report.json": true,
	"screen.txt": true, "boot.txt": true, "network-log.txt": true, "sleep-log.txt": true}

// bugLimits: a person sending a few by hand, and a bench of Macs behind one address.
type bugLimits struct {
	perAddrHour int // from one address in an hour
	perMacDay   int // from one Mac in a day (UTC)
	perDay      int // from everyone in a day (UTC)
}

var defaultBugLimits = bugLimits{perAddrHour: 12, perMacDay: 6, perDay: 500}

// bugStore keeps a bug report under a name.
type bugStore interface {
	Put(ctx context.Context, name string, body []byte) error
}

// bugs is the endpoint's setup: the upload keys (key → its name, e.g. dev or release), where
// reports go, and the limits' counts.
type bugs struct {
	keys  map[string]string
	store bugStore
	lim   bugLimits
	slots chan struct{}

	day   string         // the UTC day the counts are for (under server.mu)
	n     int            // reports taken today
	macs  map[string]int // reports taken today, per Mac
	addrs map[string]*bugUse
}

type bugUse struct {
	hour time.Time
	n    int
}

func newBugs(keys map[string]string, store bugStore, l bugLimits) *bugs {
	return &bugs{keys: keys, store: store, lim: l, slots: make(chan struct{}, bugsAtOnce), macs: map[string]int{}, addrs: map[string]*bugUse{}}
}

// readBugKeys reads the upload keys: one "name key" a line, "#" starts a comment.
func readBugKeys(file string) (map[string]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for i, l := range strings.Split(string(b), "\n") {
		l, _, _ = strings.Cut(l, "#")
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
			continue
		case len(f) != 2 || len(f[1]) < 32:
			return nil, fmt.Errorf("%s:%d: want \"name key\", a key of 32 characters or more", file, i+1)
		}
		keys[f[1]] = f[0]
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no keys", file)
	}
	return keys, nil
}

// keyName is the name of the upload key the request carries, or "".
func (b *bugs) keyName(r *http.Request) string {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	name := ""
	for k, n := range b.keys { // every key compared, in constant time
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			name = n
		}
	}
	return name
}

var macHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// bugAnswer is the answer to a stored report: the code to quote, in an issue for instance.
type bugAnswer struct {
	Code string `json:"code"`
}

func (s *server) bugreport(w http.ResponseWriter, r *http.Request) {
	b := s.bugs
	if b == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		s.bad(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	key := b.keyName(r)
	if key == "" {
		s.bad(w, http.StatusUnauthorized, "an upload key (Authorization: Bearer …)")
		return
	}
	mac := r.Header.Get("X-OmaBoot-Mac")
	if !macHash.MatchString(mac) {
		s.bad(w, http.StatusBadRequest, "X-OmaBoot-Mac: 64 lowercase hex")
		return
	}
	switch n := r.ContentLength; {
	case n < 0:
		s.bad(w, http.StatusLengthRequired, "a Content-Length")
		return
	case n > maxBug:
		s.bad(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d bytes", maxBug))
		return
	}
	if retry := s.takeBug(r, mac); retry > 0 {
		s.busy(w, retry)
		return
	}
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		s.giveBackBug(r, mac)
		s.busy(w, 30*time.Second)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBug))
	if err != nil {
		s.giveBackBug(r, mac)
		s.bad(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	code := bugCode()
	out, err := rescrub(body, code, received{Code: code, Key: key, Day: s.now().UTC().Format(time.DateOnly), Client: r.Header.Get("X-OmaBoot-Version")})
	if err != nil {
		s.giveBackBug(r, mac)
		s.bad(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := b.store.Put(ctx, "v1/"+code+".tar", out); err != nil {
		s.giveBackBug(r, mac)
		s.mu.Lock()
		s.h.BugFailed++
		s.mu.Unlock()
		http.Error(w, "bootwatchd: couldn't keep it just now, try later", http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.h.Bugs++
	s.h.BugBytes += int64(len(out))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(bugAnswer{Code: code})
}

// takeBug counts a report against the limits; the wait before another when one is reached.
func (s *server) takeBug(r *http.Request, mac string) time.Duration {
	b, a, now := s.bugs, s.addr(r), s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := now.UTC().Format(time.DateOnly); d != b.day { // a new day: the daily counts start again
		b.day, b.n, b.macs = d, 0, map[string]int{}
	}
	for k, u := range b.addrs {
		if now.Sub(u.hour) >= time.Hour {
			delete(b.addrs, k)
		}
	}
	u := b.addrs[a]
	if u == nil {
		u = &bugUse{hour: now}
		b.addrs[a] = u
	}
	tomorrow := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
	switch {
	case b.n >= b.lim.perDay, b.macs[mac] >= b.lim.perMacDay:
		s.h.Limited++
		return tomorrow
	case u.n >= b.lim.perAddrHour:
		s.h.Limited++
		return u.hour.Add(time.Hour).Sub(now)
	}
	b.n++
	b.macs[mac]++
	u.n++
	return 0
}

// giveBackBug undoes takeBug for a report that wasn't kept.
func (s *server) giveBackBug(r *http.Request, mac string) {
	b, a := s.bugs, s.addr(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	b.n = max(0, b.n-1)
	if b.macs[mac] > 0 {
		b.macs[mac]--
	}
	if u := b.addrs[a]; u != nil && u.n > 0 {
		u.n--
	}
}

// received is what bootwatchd adds to a report it keeps: its code, which key brought it, the
// day (no time, no address) and the client's version as it said.
type received struct {
	Code   string `json:"code"`
	Key    string `json:"key"`
	Day    string `json:"day"`
	Client string `json:"client,omitempty"`
}

// rescrub reads the report's tar, refuses anything but the known files, scrubs each one again
// and writes them under the report's code, with received.json.
func rescrub(body []byte, code string, rc received) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(body))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	total, seen := int64(0), map[string]bool{}
	mod := time.Now().UTC().Truncate(24 * time.Hour)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: code + "/" + name, Mode: 0o644, Size: int64(len(data)), ModTime: mod, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("body: not a tar: %v", err)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		name := path.Base(h.Name)
		switch {
		case h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA:
			return nil, fmt.Errorf("body: %s isn't a plain file", name)
		case !bugFiles[name]:
			return nil, fmt.Errorf("body: %s isn't a bug report's file", name)
		case seen[name]:
			return nil, fmt.Errorf("body: %s twice", name)
		}
		seen[name] = true
		total += h.Size
		if total > maxBugFiles {
			return nil, fmt.Errorf("body: more than %d bytes unpacked", maxBugFiles)
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("body: %v", err)
		}
		if err := put(name, []byte(scrub.Text(string(data)))); err != nil {
			return nil, err
		}
	}
	if !seen["manifest.json"] {
		return nil, errors.New("body: no manifest.json")
	}
	j, _ := json.MarshalIndent(rc, "", "  ")
	if err := put("received.json", append(j, '\n')); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// bugCode is a new report's code: B-XXXX-XXXX in Crockford's base32 (no I, L, O or U), 40
// random bits, easy to read out and to type.
func bugCode() string {
	const abc = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var r [8]byte
	rand.Read(r[:])
	c := make([]byte, 8)
	for i, x := range r {
		c[i] = abc[x&31]
	}
	return "B-" + string(c[:4]) + "-" + string(c[4:])
}

// dirStore keeps reports in a directory (development, or a server of your own).
type dirStore struct{ dir string }

func (d dirStore) Put(_ context.Context, name string, body []byte) error {
	p := filepath.Join(d.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o600)
}

// r2Store keeps reports in a Cloudflare R2 bucket over its S3 API, with a token that can only
// reach that bucket: one signed PUT per report.
type r2Store struct {
	account, keyID, secret, bucket string
	client                         *http.Client
}

// readR2 reads an R2 env file: R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET.
func readR2(file string) (*r2Store, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	v := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, x, ok := strings.Cut(strings.TrimSpace(l), "="); ok && !strings.HasPrefix(k, "#") {
			v[k] = strings.TrimSpace(x)
		}
	}
	s := &r2Store{account: v["R2_ACCOUNT_ID"], keyID: v["R2_ACCESS_KEY_ID"], secret: v["R2_SECRET_ACCESS_KEY"], bucket: v["R2_BUCKET"],
		client: &http.Client{Timeout: 20 * time.Second}}
	if s.account == "" || s.keyID == "" || s.secret == "" || s.bucket == "" {
		return nil, fmt.Errorf("%s: needs R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY and R2_BUCKET", file)
	}
	return s, nil
}

func (s *r2Store) Put(ctx context.Context, name string, body []byte) error {
	host := s.account + ".r2.cloudflarestorage.com"
	p := "/" + s.bucket + "/" + name
	now := time.Now().UTC()
	amz, day := now.Format("20060102T150405Z"), now.Format("20060102")
	sum := sha256.Sum256(body)
	ph := hex.EncodeToString(sum[:])
	signed := "content-type;host;x-amz-content-sha256;x-amz-date"
	canon := "PUT\n" + p + "\n\ncontent-type:application/x-tar\nhost:" + host + "\nx-amz-content-sha256:" + ph + "\nx-amz-date:" + amz + "\n\n" + signed + "\n" + ph
	scope := day + "/auto/s3/aws4_request"
	ch := sha256.Sum256([]byte(canon))
	sts := "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := []byte("AWS4" + s.secret)
	for _, part := range []string{day, "auto", "s3", "aws4_request"} {
		k = hmacSHA256(k, part)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+host+p, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Amz-Date", amz)
	req.Header.Set("X-Amz-Content-Sha256", ph)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.keyID+"/"+scope+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(k, sts)))
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("R2: %s", res.Status)
	}
	return nil
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}
