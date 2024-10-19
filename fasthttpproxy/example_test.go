package fasthttpproxy

import (
	"github.com/gookit/goutil/testutil/assert"
	"github.com/newacorn/fasthttp"
	"golang.org/x/net/http/httpproxy"
	"log"
	"testing"
	"time"
)

// func ExampleDialer_GetDialFunc() {
func TestA(t *testing.T) {
	d1 := Dialer{
		TCPDialer: fasthttp.TCPDialer{
			Concurrency: 1000,
		},
		ConnectTimeout: time.Second * 20,
		Config: httpproxy.Config{
			HTTPProxy:  "http://127.0.0.1:8005",
			HTTPSProxy: "http://127.0.0.1:8005",
		}}
	d2, err := d1.GetDialTimeoutFunc(false)
	assert.Nil(t, err)

	/*
		p := DialerV2{Dialer: Dialer{
			TCPDialer: fasthttp.TCPDialer{
				Concurrency: 1000,
			},
			ConnectTimeout: time.Second * 20,
			Config: httpproxy.Config{
				HTTPProxy:  "http://127.0.0.1:8005",
				HTTPSProxy: "http://127.0.0.1:8005",
			},
		},
			UseEnv: false,
		}
	*/
	client := &fasthttp.Client{
		Config: fasthttp.Config{
			ReadTimeout:         time.Second * 5,
			WriteTimeout:        time.Second * 5,
			MaxIdleConnDuration: time.Second * 5,
			Dial:                fasthttp.Dialer(d2),
		},
	}
	statusCode, resp, err := client.Get(nil, "https://www.google.com")
	if err != nil {
		log.Fatal(err)
	}
	if statusCode != 200 {
		log.Fatal(err)
	}
	_ = resp
}
