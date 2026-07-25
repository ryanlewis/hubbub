package heartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A completed HTTP request is not a fed dead-man. A typo'd or rotated check
// URL answers 404 forever; treating that as success means the switch fires
// hours later against a hub that was healthy all along.
func TestPingRejectsNon2xx(t *testing.T) {
	for _, code := range []int{404, 410, 401, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		p := &Pinger{TargetURL: srv.URL, Interval: time.Hour}
		err := p.ping(context.Background(), srv.Client())
		srv.Close()
		if err == nil {
			t.Errorf("dead-man service answered %d and the ping reported success", code)
		}
	}
}

func TestPingAccepts2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	p := &Pinger{TargetURL: srv.URL, Interval: time.Hour}
	if err := p.ping(context.Background(), srv.Client()); err != nil {
		t.Errorf("healthy ping = %v", err)
	}
}

// The tick is gated on the hub actually serving, not merely running.
func TestSelfProbeRequiresOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := &Pinger{SelfURL: srv.URL, Interval: time.Hour}
	if p.selfProbe(context.Background(), srv.Client()) {
		t.Error("a non-200 /health must withhold the ping")
	}
}
