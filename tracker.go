package main

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"

	"github.com/zeebo/bencode"
)

func GetPeersFromTracker(trackerURL string) ([]net.TCPAddr, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https":
		return getPeersFromHTTPTracker(u)
	case "udp":
		// TODO(alex) get peers via udp
		return nil, nil
	default:
		return nil, fmt.Errorf("unrecognized tracker url scheme: %s", u.Scheme)
	}
}

// httpTrackerResp is bencoded
type httpTrackerResp struct {
	Interval int    `bencode:"interval"` // time in seconds to check back for new peers
	Peers    string `bencode:"peers"`    // blob of all peer IP addresses & ports
}

func getPeersFromHTTPTracker(u *url.URL) ([]net.TCPAddr, error) {
	resp, err := http.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("sending get req to http tracker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("non-200 resp from Tracker: %d: %s", resp.StatusCode, string(b))
	}

	const peerSize = 6 // 4 bytes for IP, 2 for Port

	var trackerResp httpTrackerResp
	err = bencode.NewDecoder(resp.Body).Decode(&trackerResp)
	if err != nil || len(trackerResp.Peers)%peerSize != 0 {
		return nil, fmt.Errorf("malformed http tracker response: %w", err)
	}

	var addrs []net.TCPAddr
	for i := 0; i < len(trackerResp.Peers); i += peerSize {
		// convert port substring into byte slice to calculate via BigEndian
		portRaw := []byte(trackerResp.Peers[i+4 : i+6])
		port := binary.BigEndian.Uint16(portRaw)

		addrs = append(addrs, net.TCPAddr{
			IP:   []byte(trackerResp.Peers[i : i+4]),
			Port: int(port),
		})
	}

	return addrs, nil
}
