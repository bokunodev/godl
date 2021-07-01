package main

import (
	"bufio"
	"crypto/tls"
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
	"time"
	"unicode"

	"github.com/pkg/profile"
	"golang.org/x/net/publicsuffix"
)

var (
	DEBUG                        string
	THREADS                      uint
	URL, OUT, FILE               string
	successCounter, errorCounter int
	counterMutex                 sync.Mutex
	myCookieJar, _               = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	httpClient                   = http.Client{
		Transport: &http.Transport{
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
		Jar:     myCookieJar,
		Timeout: 0,
	}
)

type Job struct {
	io.ReadCloser
	io.WriteCloser
}

func main() {
	ballast := make([]byte, 10<<20)
	ballast[0] = 'A'

	flag.UintVar(&THREADS, "N", uint(runtime.NumCPU()), "concurent download")
	flag.StringVar(&URL, "I", "", "url to download")
	flag.StringVar(&FILE, "F", "", "downlaod from file instead")
	flag.StringVar(&OUT, "O", "", "output file")
	flag.StringVar(&DEBUG, "D", "", "debug with pprof")
	flag.Parse()

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
				_, err := io.CopyBuffer(job.WriteCloser, job.ReadCloser, buf)
				if err != nil {
					log.Println(err)
					counterMutex.Lock()
					errorCounter++
					counterMutex.Unlock()
				} else {
					counterMutex.Lock()
					successCounter++
					counterMutex.Unlock()
				}
				_ = job.WriteCloser.Close()
				_ = job.ReadCloser.Close()
			}
		}(jobCh, &wg)
	}

	defer func() {
		close(jobCh)
		wg.Wait()
		fmt.Printf("Total: %-08d Success: %-08d Error: %-08d\n", successCounter+errorCounter, successCounter, errorCounter)
	}()

	for line, err := input.readLine(); err == nil; line, err = input.readLine() {
		index := strings.Index(line, " ")
		if index < 0 {
			index = len(line)
		}

		in, out := trimWhitespace(line[:index]), trimWhitespace(line[index:])

		urlObj, err := url.Parse(in)
		if err != nil {
			log.Println(err)
			counterMutex.Lock()
			errorCounter++
			counterMutex.Unlock()
			continue
		}

		if urlObj.Scheme == "" {
			urlObj.Scheme = "http"
		}

		req, err := http.NewRequest(http.MethodGet, urlObj.String(), http.NoBody)
		if err != nil {
			log.Print(err)
			counterMutex.Lock()
			errorCounter++
			counterMutex.Unlock()
			continue
		}

		res, err := httpClient.Do(req)
		if err != nil {
			log.Print(err)
			counterMutex.Lock()
			errorCounter++
			counterMutex.Unlock()
			continue
		}

		if res.StatusCode > 299 {
			log.Println(urlObj.String(), "Server response with status:", res.StatusCode, http.StatusText(res.StatusCode))
			_ = res.Body.Close()
			counterMutex.Lock()
			errorCounter++
			counterMutex.Unlock()
			continue
		}

		if out == "" {
			out = path.Base(urlObj.Path)
			if out == "" || out == "/" {
				_, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
				var ok bool
				if out, ok = params["filename"]; err != nil || !ok {
					log.Println("Can't get filename from url or respose header", err)
					_ = res.Body.Close()
					counterMutex.Lock()
					errorCounter++
					counterMutex.Unlock()
					continue
				}
			}
		}

		outFile, err := os.OpenFile(out, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			log.Println(err)
			_ = res.Body.Close()
			counterMutex.Lock()
			errorCounter++
			counterMutex.Unlock()
			continue
		}

		jobCh <- &Job{
			ReadCloser:  res.Body,
			WriteCloser: outFile,
		}
	}
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

func trimWhitespace(s string) string {
	var start, end int
	end = len(s)
	for start < end {
		if unicode.IsLetter(rune(s[start])) {
			break
		}
		start++
	}
	for start < end {
		if unicode.IsLetter(rune(s[end-1])) {
			break
		}
		end--
	}
	return s[start:end]
}
