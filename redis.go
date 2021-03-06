package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Routines for parsing the Redis protocol.
// http://redis.io/topics/protocol

const (
	// For sanity
	redisMaxBulkStrings = 10e6
	redisMaxStringLen   = 1e6
)

var (
	ErrRedisBadBulkStringCount = errors.New("client sent an unreasonable bulk string count")
	ErrRedisBadStringLen       = errors.New("client sent an unreasonably string size")
)

func parseRedisArrayBulkString(br *bufio.Reader) ([]string, error) {
	if err := expectString(br, "*"); err != nil {
		return nil, err
	}
	count, err := expectDecimal(br)
	if err != nil {
		return nil, err
	}
	if err := expectCRLF(br); err != nil {
		return nil, err
	}
	if count < 0 || count > redisMaxBulkStrings {
		return nil, ErrRedisBadBulkStringCount
	}
	var results []string
	for i := 0; i < count; i++ {
		if err := expectString(br, "$"); err != nil {
			return nil, err
		}
		size, err := expectDecimal(br)
		if err != nil {
			return nil, err
		}
		if err := expectCRLF(br); err != nil {
			return nil, err
		}
		if size < 0 || size > redisMaxStringLen {
			return nil, ErrRedisBadStringLen
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil, err
		}
		if err := expectCRLF(br); err != nil {
			return nil, err
		}
		results = append(results, string(b))
	}
	return results, nil
}

func expectString(r io.Reader, expected string) error {
	b := make([]byte, len(expected))
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	if !bytes.Equal(b, []byte(expected)) {
		return fmt.Errorf("Expected %q; got %q", expected, b)
	}
	return nil
}

func expectCRLF(r io.Reader) error { return expectString(r, "\r\n") }

// expectDecimal expects and consumes a decimal number followed by CRLF.
func expectDecimal(br *bufio.Reader) (n int, err error) {
	line, err := br.ReadBytes('\r')
	if err != nil {
		return 0, err
	}
	if err := br.UnreadByte(); err != nil {
		return 0, err
	}
	line = line[:len(line)-1]
	n, err = strconv.Atoi(string(line))
	if err != nil {
		return 0, fmt.Errorf("Bad number: %q", line)
	}
	return n, nil
}
