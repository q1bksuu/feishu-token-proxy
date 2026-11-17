package main

import (
	"bytes"
	"errors"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("invalid handle") }

func TestTeeWriter_KeepsWritingAfterOneWriterFails(t *testing.T) {
	var file bytes.Buffer
	logger := log.New(teeWriter{failingWriter{}, &file}, "", 0)
	logger.Printf("第一行")
	logger.Printf("第二行")
	assert.Contains(t, file.String(), "第一行")
	assert.Contains(t, file.String(), "第二行")
}
