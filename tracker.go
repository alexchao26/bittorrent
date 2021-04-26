package main

import (
	"fmt"
	"net/http"

	"github.com/jackpal/bencode-go"
)

// TrackerResp is bencoded
type TrackerResp struct {
	Interval int    `bencode:"interval"` // time in seconds to check back for new peers
	Peers    string `bencode:"peers"`    // blob of all peer IP addresses & exposed ports
}

// GetPeersFromTracker via HTTP request
func GetPeersFromTracker(trackerURL string) (TrackerResp, error) {
	resp, err := http.Get(trackerURL)
	if err != nil {
		return TrackerResp{}, err
	}
	if resp.StatusCode != 200 {
		return TrackerResp{}, fmt.Errorf("GET from Tracker: %d: %s", resp.StatusCode, resp.Status)
	}
	defer resp.Body.Close()

	var trackerResp TrackerResp
	err = bencode.Unmarshal(resp.Body, &trackerResp)
	return trackerResp, err
}
