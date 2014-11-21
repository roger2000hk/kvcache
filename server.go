package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cespare/gostc"
	"github.com/dustin/go-humanize"
)

type Server struct {
	addr            string
	db              *DB
	statsd          *gostc.Client
	quitStatUpdates chan struct{}
}

func NewServer(dir, addr string, chunkSize uint64, expiry time.Duration, statsdAddr string) (*Server, error) {
	db, err := OpenDB(chunkSize, expiry, dir)
	if err != nil {
		return nil, err
	}
	statsd, err := gostc.NewClient(statsdAddr)
	if err != nil {
		return nil, err
	}
	return &Server{
		addr:            addr,
		db:              db,
		statsd:          statsd,
		quitStatUpdates: make(chan struct{}),
	}, nil
}

type Request struct {
	Type RequestType
	Key  []byte
	Val  []byte
	Resp chan *Response
	Err  error // If there was an error reading the request, only this field is set.
}

type RequestType uint8

const (
	RequestSet RequestType = iota + 1
	RequestGet
	RequestPing
	RequestInfo
)

type RedisType uint8

const (
	RedisErr RedisType = iota + 1
	RedisString
	RedisBulk
)

type Response struct {
	Type RedisType
	Msg  []byte
}

func (s *Server) Stop() error {
	close(s.quitStatUpdates)
	s.statsd.Close()
	return s.db.Close()
}

func (s *Server) Start() error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.statsd.Inc("server-start")
	go s.statUpdates()
	return s.loop(l)
}

func (s *Server) statUpdates() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			stats := s.db.Info()
			s.statsd.Gauge("db.rchunks", float64(stats.RChunks))
			s.statsd.Gauge("db.total-rlog-size", float64(stats.TotalRLogSize))
			s.statsd.Gauge("db.wlog-keys", float64(stats.WLogKeys))
			s.statsd.Gauge("db.rlog-keys", float64(stats.RLogKeys))
			s.statsd.Gauge("db.total-keys", float64(stats.TotalKeys))
		case <-s.quitStatUpdates:
			return
		}
	}
}

func (s *Server) loop(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Temporary() {
				delay := 10 * time.Millisecond
				log.Printf("Accept error: %s; retrying in %s", e, delay)
				s.statsd.Inc("errors.accept")
				time.Sleep(delay)
				continue
			}
			return err
		}
		go s.HandleConn(c)
	}
}

func when(ch chan *Response, pred bool) chan *Response {
	if pred {
		return ch
	}
	return nil
}

func head(q []*Response) *Response {
	if len(q) == 0 {
		return nil
	}
	return q[0]
}

func (s *Server) HandleConn(c net.Conn) {
	log.Printf("Client connected from %s", c.RemoteAddr())
	s.statsd.Inc("client-connect")

	// readErr and writeErr are how the request reader and response writer goroutines can notify the other that
	// the client (or connection) broke/disconnected.
	// These signal chans are only closed.
	readErr := make(chan struct{})
	writeErr := make(chan struct{})

	// This request goroutine reads requests and sends them into this goroutine to be handled and buffered;
	// Responses are sent off to the response goroutine.
	// This is necessary for Redis pipelining to work.
	requests := make(chan *Request)
	responses := make(chan *Response)

	go s.readRequests(c, requests, readErr, writeErr)
	go s.writeResponses(c, responses, readErr, writeErr)

	var responseQueue []*Response

reqLoop:
	for {
		resp := &Response{Type: RedisString}
		select {
		case r := <-requests:
			if r.Err != nil {
				s.statsd.Inc("errors.request")
				resp.Msg = []byte(r.Err.Error())
				resp.Type = RedisErr
			} else {
				s.statsd.Inc("requests")
				switch r.Type {
				case RequestSet:
					s.statsd.Inc("requests.set")
					start := time.Now()
					_, err := s.db.Put(r.Key, r.Val)
					switch err {
					case nil:
						resp.Msg = []byte("OK")
					case ErrKeyExist:
						resp.Type = RedisBulk // null
					default:
						if e, ok := err.(FatalDBError); ok {
							log.Println("Fatal DB error:", e)
							if err := s.Stop(); err != nil {
								log.Println("Error while shutting down:", err)
							}
							os.Exit(1)
						}
						resp = ResponseFromError(err)
					}
					s.statsd.Time("requests.set", time.Since(start))
				case RequestGet:
					s.statsd.Inc("requests.get")
					start := time.Now()
					v, _, err := s.db.Get(r.Key)
					switch err {
					case nil:
						resp.Type = RedisBulk
						resp.Msg = v
					case ErrKeyNotExist:
						resp.Type = RedisBulk // null
					default:
						resp = ResponseFromError(err)
					}
					s.statsd.Time("requests.get", time.Since(start))
				case RequestPing:
					s.statsd.Inc("requests.ping")
					resp.Msg = []byte("PONG")
				case RequestInfo:
					s.statsd.Inc("requests.info")
					resp.Msg = []byte(s.db.Info().String())
				default:
					s.statsd.Inc("errors.unexpected-request-type")
					panic("unexpected request type")
				}
			}
			responseQueue = append(responseQueue, resp)
		case when(responses, len(responseQueue) > 0) <- head(responseQueue):
			responseQueue = responseQueue[1:]
		case <-readErr:
			break reqLoop
		case <-writeErr:
			break reqLoop
		}
	}

	log.Printf("Client disconnected from %s", c.RemoteAddr())
	c.Close()
}

func (s *Server) readRequests(c net.Conn, requests chan<- *Request, readErr, writeErr chan struct{}) {
	br := bufio.NewReader(c)
	for {
		var r Request
		if err := r.Parse(br); err != nil {
			if _, ok := err.(net.Error); ok {
				close(readErr)
				return
			}
			r.Err = err
		}
		requests <- &r
		select {
		case <-writeErr:
			return
		default:
		}
	}
}

func (s *Server) writeResponses(c net.Conn, responses <-chan *Response, readErr, writeErr chan struct{}) {
	for {
		select {
		case resp := <-responses:
			if err := resp.Write(c); err != nil {
				close(writeErr)
				return
			}
		case <-readErr:
			return
		}
	}
}

func ResponseFromError(err error) *Response {
	return &Response{
		Type: RedisErr,
		Msg:  []byte(err.Error()),
	}
}

var (
	ErrMalformedRequest    = errors.New("malformed request")
	ErrUnrecognizedCommand = errors.New("unrecognized command")
	ErrWrongNumArgs        = errors.New("wrong number of arguments for command")
	ErrSetXXUnupported     = errors.New("the XX option to SET is not supported")
	ErrSetNXRequired       = errors.New("the NX option to SET is required")
)

func (r *Request) Parse(br *bufio.Reader) error {
	array, err := parseRedisArrayBulkString(br)
	if err != nil {
		return err
	}
	if len(array) == 0 {
		return ErrMalformedRequest
	}
	switch strings.ToUpper(array[0]) {
	case "SET":
		if len(array) < 3 {
			return ErrWrongNumArgs
		}
		r.Type = RequestSet
		r.Key = []byte(array[1])
		r.Val = []byte(array[2])
		var nx bool
		// Note permissive redis behavior:
		// https://github.com/antirez/redis/issues/2157
		for i := 3; i < len(array); i++ {
			param := strings.ToUpper(array[i])
			switch param {
			case "EX", "PX":
				if i+1 >= len(array) {
					return fmt.Errorf("expiry parameter %s provided without a value", param)
				}
				i++ // Skip the expiry value
			case "XX":
				return ErrSetXXUnupported
			case "NX":
				nx = true
			}
		}
		if !nx {
			return ErrSetNXRequired
		}
	case "GET":
		if len(array) != 2 {
			return ErrWrongNumArgs
		}
		r.Type = RequestGet
		r.Key = []byte(array[1])
	case "PING":
		if len(array) != 1 {
			return ErrWrongNumArgs
		}
		r.Type = RequestPing
	case "INFO":
		if len(array) != 1 {
			return ErrWrongNumArgs
		}
		r.Type = RequestInfo
	default:
		return ErrUnrecognizedCommand
	}
	return nil
}

func (r *Response) Write(w io.Writer) error {
	var msg []byte
	switch r.Type {
	case RedisErr:
		msg = append([]byte("-"), r.Msg...)
		msg = append(msg, "\r\n"...)
	case RedisString:
		msg = append([]byte("+"), r.Msg...)
		msg = append(msg, "\r\n"...)
	case RedisBulk:
		if r.Msg == nil {
			msg = []byte("$-1\r\n")
			break
		}
		msg = append([]byte{'$'}, strconv.Itoa(len(r.Msg))...)
		msg = append(msg, "\r\n"...)
		msg = append(msg, r.Msg...)
		msg = append(msg, "\r\n"...)
	default:
		panic("unexpected response type")
	}
	_, err := w.Write(msg)
	return err
}

func main() {
	var (
		addr       = flag.String("addr", "localhost:5533", "Listen addr")
		dir        = flag.String("dir", "db", "DB directory")
		chunkSize  = flag.String("chunksize", "100MB", "Max size for chunks")
		expiry     = flag.Duration("expiry", time.Hour, "How long data persists before expiring")
		statsdAddr = flag.String("statsdaddr", "localhost:8125", "Address to send UDP StatsD metrics")
	)
	flag.Parse()

	chunkSizeBytes, err := humanize.ParseBytes(*chunkSize)
	if err != nil {
		log.Fatalf("Bad -chunksize %q: %s", *chunkSize, err)
	}

	log.Printf("Now listening on %s (dir=%s; chunksize=%d; expiry=%s)", *addr, *dir, chunkSizeBytes, *expiry)
	server, err := NewServer(*dir, *addr, chunkSizeBytes, *expiry, *statsdAddr)
	if err != nil {
		log.Fatal(err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		log.Printf("Caught signal (%v); shutting down...", <-c)
		if err := server.Stop(); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	if err := server.Start(); err != nil {
		log.Fatal(err)
	}
}
