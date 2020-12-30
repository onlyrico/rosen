package https

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/awnumar/rosen/protocols/config"
	"github.com/awnumar/rosen/proxy"

	"github.com/hashicorp/go-retryablehttp"
	"lukechampine.com/frand"
)

// Client implements a HTTPS tunnel client.
type Client struct {
	authToken string
	remote    string
	client    *retryablehttp.RoundTripper
	proxy     *proxy.Proxy
}

// NewClient returns a new HTTPS client.
func NewClient(conf config.Configuration) (*Client, error) {
	trustPool, err := trustedCertPool(conf["pinRootCA"])
	if err != nil {
		return nil, err
	}

	client := retryablehttp.NewClient()
	client.HTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: trustPool,
			},
		},
	}
	client.Logger = logger{}

	c := &Client{
		authToken: conf["authToken"],
		remote:    conf["proxyAddr"],
		client: &retryablehttp.RoundTripper{
			Client: client,
		},
		proxy: proxy.NewProxy(),
	}

	go func(c *Client) {
		outboundBuffer := make([]proxy.Packet, clientBufferSize)

		for {
			size := c.proxy.Fill(outboundBuffer)

			responseData := c.do(outboundBuffer[:size])

			go c.proxy.Ingest(responseData)

			if size > 0 || c.proxy.QueueLen() > 0 || len(responseData) > 0 {
				continue // skip delay
			}

			time.Sleep(time.Duration(frand.Intn(100_000_000)) * time.Nanosecond)
		}
	}(c)

	return c, nil
}

func (c *Client) do(data []proxy.Packet) (responseData []proxy.Packet) {
	id := base64.RawStdEncoding.EncodeToString(frand.Bytes(16))

	payload, err := json.Marshal(data)
	if err != nil {
		panic("error: failed to encode message payload: " + err.Error())
	}

	req, err := http.NewRequest(http.MethodPost, c.remote, bytes.NewReader(payload))
	if err != nil {
		panic("error: failed to create request object: " + err.Error())
	}

	req.Header.Set("ID", id)
	req.Header.Set("Auth-Token", c.authToken)

retry:
	resp, err := c.client.RoundTrip(req) // retries on connection error or 5XX response
	if err != nil {
		errorString := "error: " + err.Error()
		if resp != nil {
			errorString += "\nstatus: " + resp.Status
			if r, err := getResponseText(resp); err == nil {
				errorString += "\nresponse: " + r
			}
		}
		panic(errorString)
	}

	respBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("error while reading server response: " + err.Error())
		goto retry
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		panic("error: server returned " + resp.Status + "\n" + string(respBytes))
	}

	if err := json.Unmarshal(respBytes, &responseData); err != nil {
		panic("error: failed to parse JSON response (is the authentication code correct?)\nerror: " + err.Error())
	}

	return
}

// ProxyConnection handles and proxies a single connection between a local client and the remote server.
func (c *Client) ProxyConnection(dest proxy.Endpoint, conn net.Conn) error {
	return c.proxy.ProxyConnection(dest, conn)
}
