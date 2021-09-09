package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/publicsuffix"
)

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

type transport struct {
	dumpHeader bool
	httpHeader
	baseTransport *http.Transport
}

func (tr *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range tr.httpHeader {
		req.Header.Set(k, v)
	}
	res, err := tr.baseTransport.RoundTrip(req)
	if err != nil {
		return res, err
	}
	if tr.dumpHeader {
		buf, err := httputil.DumpRequestOut(req, false)
		if err != nil {
			log.Println(err)
		} else {
			os.Stderr.Write(buf)
		}
		buf, err = httputil.DumpResponse(res, false)
		if err != nil {
			log.Println(err)
		} else {
			os.Stderr.Write(buf)
		}
	}
	return res, err
}

func newHttpClient(insecure, useDOH, dump, follow bool, timeout time.Duration, headers httpHeader) *http.Client {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		log.Fatal(err)
	}

	tlsClientConfig := &tls.Config{
		InsecureSkipVerify: insecure,
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transp := &transport{
		dumpHeader: dump,
		httpHeader: headers,
		baseTransport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				ip, err := resolve(addr, "A")
				if err != nil {
					ip, err = resolve(addr, "AAAA")
					if err != nil {
						log.Fatal(err)
					}
				}
				return dialer.DialContext(ctx, network, ip)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       tlsClientConfig,
		},
	}

	client := http.Client{
		Transport: transp,
		Jar:       jar,
		Timeout:   timeout,
	}

	if !follow {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &client
}

var cache = make(map[string]string)

func resolve(s string, qtype string) (string, error) {
	ip, ok := cache[s]
	if ok {
		return ip, nil
	}

	res, err := http.Get(fmt.Sprintf("https://dns.google/resolve?name=%s&type=%s&cd=true&do=false&ct=application/dns-message", s, qtype))
	if err != nil {
		return "", (err)
	}

	p, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	resmsg := dnsmessage.Message{}
	err = resmsg.Unpack(p)
	if err != nil {
		return "", err
	}

	if resmsg.RCode != dnsmessage.RCodeSuccess {
		return "", errors.New(resmsg.RCode.String())
	}

	ip = resmsg.Answers[0].Body.GoString()

	cache[s] = ip
	return ip, nil
}
