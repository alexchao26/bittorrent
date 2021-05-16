package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/zeebo/bencode"
)

func GetPeersFromTracker(trackerURL string, infoHash, peerID [20]byte, port int) ([]net.TCPAddr, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tracker URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https":
		return getPeersFromHTTPTracker(u, infoHash, peerID, port)
	case "udp":
		return getPeersFromUDPTracker(u, infoHash, peerID, port)
	default:
		return nil, fmt.Errorf("unrecognized tracker url scheme: %s", u.Scheme)
	}
}

// The compact HTTP tracker response is preferred. It is (duh) more compact
// and includes a string of peers that are 4 bytes for IP and 2 for Port, so
// six bytes per peer.
// http://bittorrent.org/beps/bep_0023.html
type compactHTTPTrackerResp struct {
	Interval int    `bencode:"interval"` // time in seconds to check back for new peers
	Peers    string `bencode:"peers"`    // blob of all peer IP addresses & ports
}

// The original HTTP tracker response was more verbose. Unfortunately, sometimes
// HTTP trackers send this format even if we ask for the compact format.
// http://bittorrent.org/beps/bep_0003.html#trackers
type originalHTTPTrackerResp struct {
	Peers []struct {
		ID   string `bencode:"peer_id"`
		IP   string `bencode:"ip"`
		Port int    `bencode:"port"`
	} `bencode:"peers"`
	// a lot of fields that are not used
	Interval   int    `bencode:"interval"`  // likely needs to be escaped
	InfoHash   string `bencode:"info_hash"` // likely needs to be escaped
	Uploaded   int    `bencode:"uploaded"`
	Downloaded int    `bencode:"downloaded"`
	Left       int    `bencode:"left"`
	Event      string `bencode:"event"`
}

func getPeersFromHTTPTracker(u *url.URL, infoHash, peerID [20]byte, port int) ([]net.TCPAddr, error) {
	v := url.Values{}
	v.Add("info_hash", string(infoHash[:]))
	// my peer_id (just random). Real bittorrent clients would identify software and version
	v.Add("peer_id", string(peerID[:]))
	v.Add("port", strconv.Itoa(port))
	v.Add("uploaded", "0")
	v.Add("downloaded", "0")
	v.Add("compact", "1") // BEP0023: compact peer list
	v.Add("left", "0")    // just say zero, if source is a magnet link we wouldn't know anyways

	// set url query params
	u.RawQuery = v.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("sending get req to http tracker: %w", err)
	}
	defer resp.Body.Close()

	// read all the bytes upfront b/c it might need to be unmarshalled into
	// the original format or the compact format
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("non-200 resp from Tracker: %d: %s", resp.StatusCode, string(raw))
	}

	// parse as compact format
	var compactResp compactHTTPTrackerResp
	err = bencode.DecodeBytes(raw, &compactResp)

	// the rare `if err == nil`, then response is in (the preferred) compact format
	if err == nil {
		var addrs []net.TCPAddr
		const peerSize = 6 // 4 bytes for IP, 2 for Port
		if len(compactResp.Peers)%peerSize != 0 {
			return nil, fmt.Errorf("malformed http tracker response: %w", err)
		}

		for i := 0; i < len(compactResp.Peers); i += peerSize {
			// convert port substring into byte slice to calculate via BigEndian
			portRaw := []byte(compactResp.Peers[i+4 : i+6])
			port := binary.BigEndian.Uint16(portRaw)

			addrs = append(addrs, net.TCPAddr{
				IP:   []byte(compactResp.Peers[i : i+4]),
				Port: int(port),
			})
		}

		return addrs, nil
	}

	// parse as original format
	var originalResp originalHTTPTrackerResp
	err = bencode.DecodeBytes(raw, &originalResp)
	if err != nil {
		return nil, fmt.Errorf("malformed http tracker response, not compact or original format: %w", err)
	}

	var addrs []net.TCPAddr
	for _, p := range originalResp.Peers {
		// assume ipv4 and not domain names. otherwise need to use net.LookupIP?
		addrs = append(addrs, net.TCPAddr{
			IP:   net.ParseIP(p.IP),
			Port: p.Port,
		})
	}

	return addrs, nil
}

func getPeersFromUDPTracker(u *url.URL, infoHash, peerID [20]byte, port int) ([]net.TCPAddr, error) {
	udpClient, err := NewUDPTrackerClient(u, infoHash, peerID, port)
	if err != nil {
		return nil, err
	}
	return udpClient.GetPeers()
}

// DedupeAddrs is a helper function to dedupe all the peer addresses received
// from multiple tracker servers
func DedupeAddrs(addrs []net.TCPAddr) []net.TCPAddr {
	deduped := []net.TCPAddr{}
	set := map[string]bool{}
	for _, a := range addrs {
		if !set[a.String()] {
			deduped = append(deduped, a)
			set[a.String()] = true
		}
	}
	return deduped
}
