package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/profile"
)

var (
	NTHREADS, NPARTS             uint
	DEBUG, URL, OUT, FILE        string
	successCounter, errorCounter uint32
	skipCertVerification,
	dump, follow, useDoh bool
	minContentLength int64 = 4 << 20 // 4MB
	headers                = make(httpHeader)
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

	flag.UintVar(&NTHREADS, "n", uint(runtime.NumCPU()), "number concurent downloads")
	flag.UintVar(&NPARTS, "p", uint(runtime.NumCPU()), "split file into parts (p)")
	flag.StringVar(&URL, "i", "", "URL to download")
	flag.StringVar(&FILE, "f", "", "download from file instead")
	flag.StringVar(&OUT, "o", "", "output file")
	flag.StringVar(&DEBUG, "D", "", "enable pprof (mem|alloc|trace|cpu)")
	flag.Var(headers, "H", "set/add http header 'Key:value'")
	flag.BoolVar(&skipCertVerification, "skip-cert-verification", false, "skip server cert verification")
	flag.BoolVar(&follow, "follow-redirection", true, "follow redirection 3xx")
	flag.BoolVar(&dump, "dump-http-header", false, "print http request and respose header")
	flag.BoolVar(&useDoh, "use-doh", false, "use dns-over-https")
	flag.Parse()
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
		case "cpu":
			p = profile.CPUProfile
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

	jobCh := make(chan *Job, NTHREADS)
	var wg sync.WaitGroup
	for i := uint(0); i < NTHREADS; i++ {
		wg.Add(1)
		go func(ch chan *Job, wg *sync.WaitGroup) {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			for job := range ch {
				_, err := copyBufferAt(job.writeAtCloser, job.ReadCloser, job.offset, buf)
				if err != nil {
					log.Println(err)
					atomic.AddUint32(&errorCounter, 1)
				} else {
					atomic.AddUint32(&successCounter, 1)
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

	client := newHttpClient(
		skipCertVerification,
		useDoh, dump, follow, 10*time.Second, headers)

	// main loop
	for line, err := input.readLine(); err == nil; line, err = input.readLine() {
		index := strings.Index(line, " ")
		if index < 0 {
			index = len(line)
		}

		in, out := strings.TrimSpace(line[:index]), strings.TrimSpace(line[index:])

		urlObj, err := url.Parse(in)
		if err != nil {
			log.Println(err)
			atomic.AddUint32(&errorCounter, 1)
			continue
		}

		if urlObj.Scheme == "" {
			urlObj.Scheme = "http"
		}

		req, err := http.NewRequest(http.MethodGet, urlObj.String(), http.NoBody)
		if err != nil {
			log.Println(err)
			atomic.AddUint32(&errorCounter, 1)
			continue
		}

		res, err := client.Do(req)
		if err != nil {
			log.Println(err)
			_ = req.Body.Close()
			atomic.AddUint32(&errorCounter, 1)
			continue
		}

		if res.StatusCode > 299 {
			log.Println(urlObj.String(), "Server response with status:", res.StatusCode, http.StatusText(res.StatusCode))
			_ = res.Body.Close()
			_ = req.Body.Close()
			atomic.AddUint32(&errorCounter, 1)
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
					_ = req.Body.Close()
					atomic.AddUint32(&errorCounter, 1)
					continue
				}
			}
		}

		jobs, err := splitRequest(client, req, res, out, int(NPARTS))
		if err != nil {
			log.Println(err)
			continue
		}

		for _, each := range jobs {
			jobCh <- each
		}
	}
	// end main loop
}

/* start lineReader type */
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

/* end lineReader type */

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

func splitRequest(client *http.Client, req *http.Request, res *http.Response, fname string, nparts int) ([]*Job, error) {
	var (
		jobs    []*Job
		outFile *os.File
		err     error
	)

	outFile, err = os.OpenFile(fname, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		_ = res.Body.Close()
		_ = req.Body.Close()
		atomic.AddUint32(&errorCounter, 1)
		return nil, err
	}

	if nparts < 2 || !acceptRanges(res) {
		jobs = append(jobs, &Job{
			ReadCloser:    res.Body,
			writeAtCloser: outFile,
			offset:        0,
			wg:            nil,
		})
		_ = req.Body.Close()
		return jobs, nil
	}

	quotient, reminder := res.ContentLength/int64(nparts), res.ContentLength%int64(nparts)
	fileWG := &sync.WaitGroup{}
	for i := int64(0); i < int64(nparts); i++ {
		start := quotient * i
		end := start + quotient
		req2 := req.Clone(context.Background())
		req2.Header["Range"] = []string{fmt.Sprintf("bytes=%d-%d", start, end)}
		res2, err := client.Do(req2)
		if err != nil {
			log.Println(err)
			_ = req2.Body.Close()
			continue
		}

		fileWG.Add(1)

		jobs = append(jobs, &Job{
			ReadCloser:    res2.Body,
			writeAtCloser: outFile,
			offset:        start,
			wg:            fileWG,
		})
	}
	if reminder > 0 {
		start := quotient * int64(nparts)
		req3 := req.Clone(context.Background())
		req3.Header["Range"] = []string{fmt.Sprintf("bytes=%d-", start)}
		res3, err := client.Do(req3)
		if err != nil {
			log.Println(err)
			_ = req3.Body.Close()
			return nil, err
		}

		fileWG.Add(1)

		jobs = append(jobs, &Job{
			ReadCloser:    res3.Body,
			writeAtCloser: outFile,
			offset:        start,
			wg:            fileWG,
		})
	}

	// outFile will be closed by this goroutine.
	// if main is return earlier, it'll be cleaned up by the OS.
	go func(file *os.File, wg *sync.WaitGroup) {
		wg.Wait()
		_ = file.Close()
	}(outFile, fileWG)

	return jobs, err
}
