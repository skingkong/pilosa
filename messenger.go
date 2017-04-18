package pilosa

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"

	"golang.org/x/sync/errgroup"

	"net"

	"github.com/gogo/protobuf/proto"
)

// HTTPBroadcaster represents a NodeSet that broadcasts messages over HTTP.
type HTTPBroadcaster struct {
	server       *Server
	internalPort string
}

// NewHTTPBroadcaster returns a new instance of HTTPBroadcaster.
func NewHTTPBroadcaster(s *Server, internalPort string) *HTTPBroadcaster {
	return &HTTPBroadcaster{server: s}
}

// SendSync sends a protobuf message to all nodes simultaneously.
// It waits for all nodes to respond before the function returns (and returns any errors).
func (h *HTTPBroadcaster) SendSync(pb proto.Message) error {
	// Marshal the pb to []byte
	buf, err := MarshalMessage(pb)
	if err != nil {
		return err
	}

	nodes, err := h.nodes()
	if err != nil {
		return err
	}

	var g errgroup.Group
	for _, n := range nodes {
		// Don't send the message to the local node.
		if n.Host == h.server.Host {
			continue
		}
		node := n
		g.Go(func() error {
			return h.sendNodeMessage(node, buf)
		})
	}
	return g.Wait()
}

// SendAsync exists to implement the Broadcaster interface, but just calls
// SendSync.
func (h *HTTPBroadcaster) SendAsync(pb proto.Message) error {
	return h.SendSync(pb)
}

func (h *HTTPBroadcaster) nodes() ([]*Node, error) {
	if h.server == nil {
		return nil, errors.New("HTTPBroadcaster has no reference to Server.")
	}
	nodeset, ok := h.server.Cluster.NodeSet.(*HTTPNodeSet)
	if !ok {
		return nil, errors.New("NodeSet cannot be caste to HTTPNodeSet.")
	}
	return nodeset.Nodes(), nil
}

func (h *HTTPBroadcaster) sendNodeMessage(node *Node, msg []byte) error {
	var client *http.Client
	client = http.DefaultClient

	host, _, err := net.SplitHostPort(node.Host)

	// Create HTTP request.
	req, err := http.NewRequest("POST", (&url.URL{
		Scheme: "http",
		Host:   host + ":" + h.internalPort,
	}).String(), bytes.NewReader(msg))
	if err != nil {
		return err
	}

	// Require protobuf encoding.
	req.Header.Set("Content-Type", "application/x-protobuf")

	// Send request to remote node.
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Read response into buffer.
	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return err
	}

	// Check status code.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid status: code=%d, err=%s", resp.StatusCode, body)
	}

	return nil
}

type HTTPBroadcastReceiver struct {
	port      string
	handler   BroadcastHandler
	logOutput io.Writer
}

func NewHTTPBroadcastReceiver(port string, logOutput io.Writer) *HTTPBroadcastReceiver {
	return &HTTPBroadcastReceiver{
		port:      port,
		logOutput: logOutput,
	}
}

func (rec *HTTPBroadcastReceiver) Start(b BroadcastHandler) error {
	rec.handler = b
	go func() {
		err := http.ListenAndServe(":"+rec.port, rec)
		if err != nil {
			fmt.Fprintf(rec.logOutput, "Error listening on %v for HTTPBroadcastReceiver: %v\n", ":"+rec.port, err)
		}
	}()
	return nil
}

func (rec *HTTPBroadcastReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "Unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	// Read entire body.
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Unmarshal message to specific proto type.
	m, err := UnmarshalMessage(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := rec.handler.ReceiveMessage(m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
}
