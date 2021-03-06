package direct

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/cloudflare/golibs/lrucache"
	"github.com/phuslu/glog"

	"../../dialer"
	"../../filters"
	"../../helpers"
	"../../storage"
)

const (
	filterName string = "direct"
)

type Config struct {
	Transport struct {
		Dialer struct {
			Timeout        int
			KeepAlive      int
			DualStack      bool
			RetryTimes     int
			RetryDelay     float32
			DNSCacheExpiry int
			DNSCacheSize   uint
		}
		TLSClientConfig struct {
			InsecureSkipVerify     bool
			ClientSessionCacheSize int
		}
		DisableKeepAlives   bool
		DisableCompression  bool
		TLSHandshakeTimeout int
		MaxIdleConnsPerHost int
	}
}

type Filter struct {
	Config
	filters.RoundTripFilter
	transport *http.Transport
}

func init() {
	filename := filterName + ".json"
	config := new(Config)
	err := storage.ReadJsonConfig(storage.LookupConfigStoreURI(filterName), filename, config)
	if err != nil {
		glog.Fatalf("storage.ReadJsonConfig(%#v) failed: %s", filename, err)
	}

	err = filters.Register(filterName, &filters.RegisteredFilter{
		New: func() (filters.Filter, error) {
			return NewFilter(config)
		},
	})

	if err != nil {
		glog.Fatalf("Register(%#v) error: %s", filterName, err)
	}
}

func NewFilter(config *Config) (filters.Filter, error) {
	d := &dialer.Dialer{
		Dialer: net.Dialer{
			KeepAlive: time.Duration(config.Transport.Dialer.KeepAlive) * time.Second,
			Timeout:   time.Duration(config.Transport.Dialer.Timeout) * time.Second,
			DualStack: config.Transport.Dialer.DualStack,
		},
		RetryTimes:     config.Transport.Dialer.RetryTimes,
		RetryDelay:     time.Duration(config.Transport.Dialer.RetryDelay*1000) * time.Second,
		DNSCache:       lrucache.NewLRUCache(config.Transport.Dialer.DNSCacheSize),
		DNSCacheExpiry: time.Duration(config.Transport.Dialer.DNSCacheExpiry) * time.Second,
		LoopbackAddrs:  make(map[string]struct{}),
	}

	d.LoopbackAddrs = make(map[string]struct{})
	d.LoopbackAddrs["127.0.0.1"] = struct{}{}
	d.LoopbackAddrs["::1"] = struct{}{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			switch addr.Network() {
			case "ip":
				d.LoopbackAddrs[addr.String()] = struct{}{}
			}
		}
	}

	tr := &http.Transport{
		Dial: d.Dial,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: config.Transport.TLSClientConfig.InsecureSkipVerify,
			ClientSessionCache: tls.NewLRUClientSessionCache(config.Transport.TLSClientConfig.ClientSessionCacheSize),
		},
		TLSHandshakeTimeout: time.Duration(config.Transport.TLSHandshakeTimeout) * time.Second,
		MaxIdleConnsPerHost: config.Transport.MaxIdleConnsPerHost,
		DisableCompression:  config.Transport.DisableCompression,
	}

	return &Filter{
		Config:    *config,
		transport: tr,
	}, nil
}

func (f *Filter) FilterName() string {
	return filterName
}

func (f *Filter) RoundTrip(ctx *filters.Context, req *http.Request) (*filters.Context, *http.Response, error) {
	switch req.Method {
	case "CONNECT":
		glog.V(2).Infof("%s \"DIRECT %s %s %s\" - -", req.RemoteAddr, req.Method, req.Host, req.Proto)
		rconn, err := f.transport.Dial("tcp", req.Host)
		if err != nil {
			return ctx, nil, err
		}

		rw := ctx.GetResponseWriter()

		hijacker, ok := rw.(http.Hijacker)
		if !ok {
			return ctx, nil, fmt.Errorf("http.ResponseWriter(%#v) does not implments http.Hijacker", rw)
		}

		flusher, ok := rw.(http.Flusher)
		if !ok {
			return ctx, nil, fmt.Errorf("http.ResponseWriter(%#v) does not implments http.Flusher", rw)
		}

		rw.WriteHeader(http.StatusOK)
		flusher.Flush()

		lconn, _, err := hijacker.Hijack()
		if err != nil {
			return ctx, nil, fmt.Errorf("%#v.Hijack() error: %v", hijacker, err)
		}
		defer lconn.Close()

		go helpers.IoCopy(rconn, lconn)
		helpers.IoCopy(lconn, rconn)

		ctx.Hijack(true)
		return ctx, nil, nil
	default:
		resp, err := f.transport.RoundTrip(req)

		if err != nil {
			glog.Errorf("%s \"DIRECT %s %s %s\" error: %s", req.RemoteAddr, req.Method, req.URL.String(), req.Proto, err)
			data := err.Error()
			resp = &http.Response{
				Status:        "502 Bad Gateway",
				StatusCode:    502,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
				Header:        http.Header{},
				Request:       req,
				Close:         true,
				ContentLength: int64(len(data)),
				Body:          ioutil.NopCloser(bytes.NewReader([]byte(data))),
			}
			err = nil
		} else {
			if req.RemoteAddr != "" {
				glog.V(2).Infof("%s \"DIRECT %s %s %s\" %d %s", req.RemoteAddr, req.Method, req.URL.String(), req.Proto, resp.StatusCode, resp.Header.Get("Content-Length"))
			}
		}
		return ctx, resp, err
	}
}
