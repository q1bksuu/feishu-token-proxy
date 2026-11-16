package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("invalid handle") }

func TestTeeWriter_KeepsWritingAfterOneWriterFails(t *testing.T) {
	var file bytes.Buffer
	logger := log.New(teeWriter{failingWriter{}, &file}, "", 0)
	logger.Printf("第一行")
	logger.Printf("第二行")
	if got := file.String(); !strings.Contains(got, "第一行") || !strings.Contains(got, "第二行") {
		t.Errorf("file = %q, want both lines", got)
	}
}
