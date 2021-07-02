package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/profile"
	"golang.org/x/net/publicsuffix"
)

var (
	THREADS                      uint
	DEBUG, URL, OUT, FILE        string
	successCounter, errorCounter int32
	insecureSkipVerify           bool
	followRedirection            bool
	myCookieJar, _               = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})

	minContentLength int64 = 4 << 20 // 4MB

	myHttpHeader = make(httpHeader)
	myTransport  = &transport{
		httpHeader: myHttpHeader,
		base: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	httpClient = http.Client{
		Transport: myTransport,
		Jar:       myCookieJar,
		Timeout:   0,
	}
)

type writeAtCloser interface {
	WriteAt(b []byte, off int64) (n int, err error)
	io.Closer
}

type Job struct {
	io.ReadCloser
	writeAtCloser
	offset int64
	wg     *sync.WaitGroup
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(os.Stderr)

	flag.UintVar(&THREADS, "N", uint(runtime.NumCPU()), "Number concurent downloads")
	flag.StringVar(&URL, "I", "", "URL to download")
	flag.StringVar(&FILE, "F", "", "Download from file instead")
	flag.StringVar(&OUT, "O", "", "Output file")
	flag.StringVar(&DEBUG, "D", "", "Debug with pprof (mem|alloc|trace)")
	flag.Var(myHttpHeader, "H", "Set/Add http header 'Key:value'")
	flag.BoolVar(&insecureSkipVerify, "skip-cert-verification", false, "Skip server cert verification")
	flag.BoolVar(&followRedirection, "follow-redirection", true, "Follow http redrection 3xx")
	flag.Parse()

	myTransport.base.TLSClientConfig.InsecureSkipVerify = insecureSkipVerify
	if !followRedirection {
		httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
}

func main() {
	if DEBUG != "" {
		var p func(*profile.Profile)
		switch DEBUG {
		case "mem":
			p = profile.MemProfile
		case "trace":
			p = profile.TraceProfile
		case "alloc":
			p = profile.MemProfileAllocs
		default:
			log.Printf("Unrecognized profile '%s'\n", DEBUG)
			os.Exit(1)
		}
		defer profile.Start(
			profile.ProfilePath("."),
			profile.MemProfileRate(1),
			p).Stop()
	}

	if URL == "" && FILE == "" {
		fmt.Println(`¯\_(ツ)_/¯ I have nothing to do ...`)
		return
	}

	var input *lineReader = newLineReader(strings.NewReader(URL + " " + OUT))
	if FILE != "" {
		if FILE == "-" || FILE == "stdin" {
			input = newLineReader(os.Stdin)
		} else {
			file, err := os.Open(FILE)
			if err != nil {
				log.Fatal(err)
			}
			input = newLineReader(file)
			defer file.Close()
		}
	}

	jobCh := make(chan *Job, THREADS)
	var wg sync.WaitGroup
	for i := uint(0); i < THREADS; i++ {
		wg.Add(1)
		go func(ch chan *Job, wg *sync.WaitGroup) {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			for job := range ch {
				_, err := copyBufferAt(job.writeAtCloser, job.ReadCloser, job.offset, buf)
				if err != nil {
					log.Println(err)
					updateError()
				} else {
					updateSuccess()
				}
				if job.wg == nil {
					_ = job.writeAtCloser.Close()
				} else {
					job.wg.Done()
				}
				_ = job.ReadCloser.Close()
			}
		}(jobCh, &wg)
	}

	defer func() {
		close(jobCh)
		wg.Wait()
		fmt.Printf("Total: %-07d Success: %-07d Error: %-07d\n", successCounter+errorCounter, successCounter, errorCounter)
	}()

main_loop: /* start main_loop */
	for line, err := input.readLine(); err == nil; line, err = input.readLine() {
		index := strings.Index(line, " ")
		if index < 0 {
			index = len(line)
		}

		in, out := strings.TrimSpace(line[:index]), strings.TrimSpace(line[index:])

		urlObj, err := url.Parse(in)
		if err != nil {
			log.Println(err)
			updateError()
			continue main_loop
		}

		if urlObj.Scheme == "" {
			urlObj.Scheme = "http"
		}

		req, err := http.NewRequest(http.MethodGet, urlObj.String(), http.NoBody)
		if err != nil {
			log.Println(err)
			updateError()
			continue main_loop
		}

		res, err := httpClient.Do(req)
		if err != nil {
			log.Println(err)
			_ = req.Body.Close()
			updateError()
			continue main_loop
		}

		if res.StatusCode > 299 {
			log.Println(urlObj.String(), "Server response with status:", res.StatusCode, http.StatusText(res.StatusCode))
			_ = res.Body.Close()
			_ = req.Body.Close()
			updateError()
			continue main_loop
		}

		if out == "" {
			out = path.Base(urlObj.Path)
			if out == "" || out == "/" {
				_, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
				var ok bool
				if out, ok = params["filename"]; err != nil || !ok {
					log.Println("Can't get filename from url or respose header", err)
					_ = res.Body.Close()
					_ = req.Body.Close()
					updateError()
					continue main_loop
				}
			}
		}

		outFile, err := os.OpenFile(out, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			log.Println(err)
			_ = res.Body.Close()
			_ = req.Body.Close()
			updateError()
			continue main_loop
		}

		if THREADS < 2 || !acceptRanges(res) {
			jobCh <- &Job{
				ReadCloser:    res.Body,
				writeAtCloser: outFile,
				offset:        0,
				wg:            nil,
			}
			_ = req.Body.Close()
			continue main_loop
		}

		quotient, reminder := res.ContentLength/int64(THREADS), res.ContentLength%int64(THREADS)
		fileWG := new(sync.WaitGroup)
	second_loop:
		for i := int64(0); i < int64(THREADS); i++ {
			start := quotient * i
			end := start + quotient
			req2 := req.Clone(context.Background())
			req2.Header["Range"] = []string{fmt.Sprintf("bytes=%d-%d", start, end)}
			res2, err := httpClient.Do(req2)
			if err != nil {
				log.Println(err)
				_ = req2.Body.Close()
				continue second_loop
			}

			fileWG.Add(1)

			jobCh <- &Job{
				ReadCloser:    res2.Body,
				writeAtCloser: outFile,
				offset:        start,
				wg:            fileWG,
			}
		}

		if reminder > 0 {
			start := quotient * int64(THREADS)
			req3 := req.Clone(context.Background())
			req3.Header["Range"] = []string{fmt.Sprintf("bytes=%d-", start)}
			res3, err := httpClient.Do(req3)
			if err != nil {
				log.Println(err)
				_ = req3.Body.Close()
				continue main_loop
			}

			fileWG.Add(1)

			jobCh <- &Job{
				ReadCloser:    res3.Body,
				writeAtCloser: outFile,
				offset:        start,
				wg:            fileWG,
			}
		}

		// outFile will be closed by this goroutine.
		// if main is return earlier, it'll be cleaned up by the OS.
		go func(file *os.File, wg *sync.WaitGroup) {
			wg.Wait()
			_ = file.Close()
		}(outFile, fileWG)
	} /* end main_loop */
}

type transport struct {
	httpHeader
	base *http.Transport
}

func (tr *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range tr.httpHeader {
		req.Header.Set(k, v)
	}
	return tr.base.RoundTrip(req)
}

type httpHeader map[string]string

func (h httpHeader) Set(s string) error {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return fmt.Errorf("Invalid arggument value, http header '%s'\n", s)
	}
	h[strings.TrimSpace(s[:idx])] = strings.TrimSpace(s[idx:])
	return nil
}

func (h httpHeader) String() string {
	sb := strings.Builder{}
	for k, v := range h {
		sb.WriteString(k)
		sb.Write([]byte{':', ' '})
		sb.WriteString(v)
		sb.WriteByte('\n')
	}
	return sb.String()
}

type lineReader struct {
	bufio.Reader
	strings.Builder
}

func newLineReader(r io.Reader) *lineReader {
	lr := &lineReader{Reader: *bufio.NewReader(r)}
	lr.Builder.Grow(1 << 20)
	return lr
}

func (lr *lineReader) readLine() (string, error) {
	lr.Builder.Reset()
	var err error
	var prefix bool
	var line []byte
	for {
		line, prefix, err = lr.Reader.ReadLine()
		lr.Builder.Write(line)
		if err != nil {
			break
		}
		if !prefix {
			break
		}
	}
	return lr.Builder.String(), err
}

func acceptRanges(res *http.Response) bool {
	_, ok := res.Header["Accept-Ranges"]
	return ok && res.ContentLength > minContentLength
}

var errInvalidWrite = errors.New("invalid write result")

func copyBufferAt(dst io.WriterAt, src io.Reader, offset int64, buf []byte) (written int64, err error) {
	if buf == nil {
		buf = make([]byte, 32<<10)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.WriteAt(buf[0:nr], offset)
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = errInvalidWrite
				}
			}
			written += int64(nw)
			offset += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

func updateError() {
	atomic.AddInt32(&errorCounter, 1)
}

func updateSuccess() {
	atomic.AddInt32(&successCounter, 1)
}
