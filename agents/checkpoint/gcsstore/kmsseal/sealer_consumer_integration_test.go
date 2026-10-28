//go:build integration

/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package kmsseal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const consumerMessageLimit = 64 << 20

type consumerMessage struct {
	Operation string `json:"operation"`
	Data      []byte `json:"data"`
}

type consumerReply struct {
	Data  []byte `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type consumerOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *consumerOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 4<<20 {
		b.overflow = true
		p = p[:max(0, 4<<20-b.Len())]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// TestSealerConsumerProcess keeps the package-owned client binding out of the
// production API. The consumer's Sealer forwards opaque calls to this instance;
// this exercises payload AEAD and caller binding, not New, ADC or Cloud KMS IAM.
// Build both binaries first, then run only this parent test:
//
//	(cd ecosystems/axlotl/mentat-wq && go test -tags=integration -c -o /absolute/consumer.test ./processor/cmd)
//	(cd public/go-driftlessaf && go test -tags=integration -c -o /absolute/sealer.test ./agents/checkpoint/gcsstore/kmsseal)
//	KMSSEAL_CONSUMER_BINARY=/absolute/consumer.test KMSSEAL_CONSUMER_TEST=TestAuthenticatedGemCustodyChild \
//	 KMSSEAL_CONSUMER_CWD=/absolute/mono/ecosystems/axlotl/mentat-wq/processor/cmd \
//	 KMSSEAL_CONSUMER_FIXTURES=/absolute/mono/ecosystems/common/gem/testdata \
//	 /absolute/sealer.test -test.run='^TestSealerConsumerProcess$' -test.v -test.timeout=10m
//
// The tests never invoke Go or compile their consumer. The working directory and
// fixture path are explicit because the consumer belongs to another module.
func TestSealerConsumerProcess(t *testing.T) {
	binary := os.Getenv("KMSSEAL_CONSUMER_BINARY")
	name := os.Getenv("KMSSEAL_CONSUMER_TEST")
	cwd := os.Getenv("KMSSEAL_CONSUMER_CWD")
	fixtures := os.Getenv("KMSSEAL_CONSUMER_FIXTURES")
	if binary == "" && name == "" && cwd == "" && fixtures == "" {
		t.Skip("consumer process test is not configured")
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(cwd) || !filepath.IsAbs(fixtures) || !regexp.MustCompile(`^Test[A-Za-z0-9_]+$`).MatchString(name) {
		t.Fatal("supply an absolute prebuilt consumer, explicit cwd/fixtures and one Test name as documented above")
	}
	fi, err := os.Lstat(binary)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("consumer binary: got = (%v, %v), want an executable regular file", fi, err)
	}
	var secretBytes [32]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(secretBytes[:])
	kms := &fakeKMS{pad: 0x5a}
	sealer := testSealer(kms)
	var mu sync.Mutex
	var seals, opens, openFailures int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/checkpoint" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
			http.Error(w, "unauthorized test peer", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, consumerMessageLimit)
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var message consumerMessage
		if err := decoder.Decode(&message); err != nil {
			http.Error(w, "invalid bounded request", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "trailing request data", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var data []byte
		var err error
		switch message.Operation {
		case "seal":
			seals++
			data, err = sealer.Seal(r.Context(), message.Data)
		case "open":
			opens++
			data, err = sealer.Open(r.Context(), message.Data)
			if err != nil {
				openFailures++
			}
		default:
			http.Error(w, "unknown operation", http.StatusBadRequest)
			return
		}
		response := consumerReply{Data: data}
		if err != nil {
			response.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	srv.Config.ReadHeaderTimeout = 5 * time.Second
	srv.Config.ReadTimeout = 30 * time.Second
	srv.Config.WriteTimeout = 30 * time.Second
	srv.Start()
	defer srv.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.v=true", "-test.timeout=7m")
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=/usr/bin:/bin", "KMSSEAL_CONSUMER_CHILD=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "BROWSER=/bin/false", "CHAINGUARD_DEFAULT_SKIP_AUTO_LOGIN=true"}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	cmd.ExtraFiles = []*os.File{reader}
	cmd.WaitDelay = 5 * time.Second
	var output consumerOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	_ = reader.Close()
	bootstrap := struct {
		Endpoint string `json:"endpoint"`
		Secret   string `json:"secret"`
		Fixtures string `json:"fixtures"`
	}{Endpoint: srv.URL + "/checkpoint", Secret: secret, Fixtures: fixtures}
	if err := json.NewEncoder(writer).Encode(bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	text := output.String()
	if err != nil || output.overflow || !strings.Contains(text, "=== RUN   "+name+"\n") || !strings.Contains(text, "--- PASS: "+name+" (") || strings.Contains(text, "--- SKIP:") || strings.Contains(text, "no tests to run") {
		t.Fatalf("consumer: got = (%v, overflow=%t), want actual RUN/PASS without skips:\n%s", err, output.overflow, text)
	}
	t.Logf("consumer output:\n%s", text)
	mu.Lock()
	defer mu.Unlock()
	if seals == 0 || opens == 0 || openFailures == 0 || len(kms.encrypts) != seals || len(kms.decrypts) < opens-openFailures || len(kms.decrypts) > opens {
		t.Fatalf("actual crypto calls: got = seals %d opens %d rejected %d wraps %d unwraps %d, want all consumer operations and an authenticated rejection", seals, opens, openFailures, len(kms.encrypts), len(kms.decrypts))
	}
	for _, request := range kms.encrypts {
		if request.Name != testKEK || !bytes.Equal(request.AdditionalAuthenticatedData, []byte(dekWrapAAD)) || len(request.Plaintext) != 32 {
			t.Fatal("Seal did not use the existing KEK, purpose AAD and AES-256 DEK")
		}
	}
	for _, request := range kms.decrypts {
		if request.Name != testKEK || !bytes.Equal(request.AdditionalAuthenticatedData, []byte(dekWrapAAD)) {
			t.Fatal("Open did not use the same KEK and purpose AAD")
		}
	}
	t.Logf("consumer actually passed; real Sealer calls seal=%d open=%d authenticated-rejections=%d; fake KMS transport is not cloud qualification", seals, opens, openFailures)
}
