// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package ninja

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/charmbracelet/log"
	"go.chromium.org/infra/build/siso/build"
)

func newStatuszServer(ctx context.Context, b *build.Builder) error {
	mux := http.NewServeMux()

	mux.Handle("/api/active_steps", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		activeSteps := b.ActiveSteps()
		buf, err := json.Marshal(activeSteps)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to json marshal: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Add("Context-Type", "text/json")
		_, err = w.Write(buf)
		if err != nil {
			log.Warnf("failed to write response: %v", err)
		}
	}))
	s := &http.Server{
		Handler: mux,
	}
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Warnf("listener error: %v", err)
		return err
	}
	defer func() {
		err := listener.Close()
		if err != nil {
			log.Warnf("listener close error: %v", err)
		}
	}()

	s.Addr = listener.Addr().String()
	log.Infof("statusz listening on port %s", s.Addr)
	err = os.WriteFile(".siso_port", []byte(s.Addr), 0644)
	if err != nil {
		log.Warnf("failed to write .siso_port: %v", err)
	}
	defer func() {
		err := os.Remove(".siso_port")
		if err != nil {
			log.Warnf("failed to remove .siso_port: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		err := s.Close()
		if err != nil {
			log.Warnf("http close error: %v", err)
		}
	}()

	err = s.Serve(listener)
	if err != nil {
		log.Warnf("http serve error: %v", err)
	}
	return nil
}
