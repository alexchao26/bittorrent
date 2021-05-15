package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"time"
)

type Action uint32

const (
	ConnectAction Action = iota
	AnnounceAction
	ScrapeAction
	ErrorAction
)

type UDPTrackerClient struct {
	conn         net.Conn
	peerID       [20]byte
	infoHash     [20]byte
	peers        []net.TCPAddr
	connectionID uint64
}

// NewUDPTrackerClient can acquire Peer IP addressed (and ports) from a UDP
// Tracker Server. It is used with magnet links to locate peers without DHT.
//
// BEP0015 spec: http://bittorrent.org/beps/bep_0015.html
func NewUDPTrackerClient(trURL *url.URL, infoHash [20]byte) (*UDPTrackerClient, error) {
	conn, err := net.DialTimeout("udp", trURL.Host, time.Second*5)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", trURL.String(), err)
	}

	var peerID [20]byte
	rand.Read(peerID[:])

	return &UDPTrackerClient{
		conn:     conn,
		peerID:   peerID,
		infoHash: infoHash,
	}, nil
}

// GetPeers connects and announces to the UDP tracker, then returns the peer addresses
func (u *UDPTrackerClient) GetPeers() ([]net.TCPAddr, error) {
	err := u.connect()
	if err != nil {
		return nil, err
	}
	err = u.announce()
	if err != nil {
		return nil, err
	}
	return u.peers, nil
}

// Connect is the first message sent to a UDP Tracker Server to acquire a Connection ID
// which is stored on the UDPTrackerClient for the AnnounceAction request
func (u *UDPTrackerClient) connect() error {
	const protocolID = 0x41727101980 // magic constant
	transactionID := rand.Uint32()

	announceMsg := make([]byte, 8+4+4)
	binary.BigEndian.PutUint64(announceMsg[0:], uint64(protocolID))
	binary.BigEndian.PutUint32(announceMsg[8:], uint32(ConnectAction))
	binary.BigEndian.PutUint32(announceMsg[12:], transactionID)

	// fmt.Println("connect msg", announceMsg)

	// set a deadline
	u.conn.SetDeadline(time.Now().Add(time.Second * 3))

	_, err := u.conn.Write(announceMsg)
	if err != nil {
		return fmt.Errorf("writing to udp connection: %w", err)
	}

	// Read connect response/output from server
	buf := make([]byte, 16)
	n, err := io.ReadFull(u.conn, buf)
	if err != nil {
		return fmt.Errorf("reading: %w", err)
	}
	if n != 16 {
		return fmt.Errorf("connect response, want 16 bytes, got %d", n)
	}

	respAction := binary.BigEndian.Uint32(buf[0:4])
	respTransactionID := binary.BigEndian.Uint32(buf[4:8])
	if respAction != uint32(ConnectAction) {
		return fmt.Errorf("expected action 0, connect; got %d", respAction)
	}
	if respTransactionID != transactionID {
		return fmt.Errorf("transactionIDs do not match, want %d got %d", transactionID, respTransactionID)
	}

	u.connectionID = binary.BigEndian.Uint64(buf[8:16])
	// fmt.Printf("connectionID is %d\n", u.connectionID)

	return nil
}

func (u *UDPTrackerClient) announce() error {
	announceMsg := make([]byte, 98)

	binary.BigEndian.PutUint64(announceMsg[0:8], u.connectionID)
	binary.BigEndian.PutUint32(announceMsg[8:12], uint32(AnnounceAction))
	transactionID := rand.Uint32()
	binary.BigEndian.PutUint32(announceMsg[12:16], transactionID)
	copy(announceMsg[16:36], u.infoHash[:])

	// random peer id
	copy(announceMsg[36:56], u.peerID[:])

	binary.BigEndian.PutUint64(announceMsg[56:64], 0) // downloaded
	binary.BigEndian.PutUint64(announceMsg[64:72], 0) // left // todo we kind of don't know this upfront?!
	binary.BigEndian.PutUint64(announceMsg[72:80], 0) // uploaded

	binary.BigEndian.PutUint32(announceMsg[80:84], 0) // event 0:none; 1:completed; 2:started; 3:stopped
	binary.BigEndian.PutUint32(announceMsg[84:88], 0) // IP address, default: 0

	binary.BigEndian.PutUint32(announceMsg[88:92], rand.Uint32()) // key - not really sure what this is

	// trick language into wrapping a negative unsigned int, it underflows
	neg1 := -1
	binary.BigEndian.PutUint32(announceMsg[92:96], uint32(neg1)) // num_want -1 default
	binary.BigEndian.PutUint16(announceMsg[96:98], uint16(6881)) // port

	u.conn.SetDeadline(time.Now().Add(time.Second * 5))
	defer u.conn.SetDeadline(time.Time{}) // clear deadlines

	// fmt.Println("writing announce message", announceMsg)
	_, err := u.conn.Write(announceMsg)
	if err != nil {
		return fmt.Errorf("writing announce msg: %w", err)
	}

	// don't know upfront how many bytes the message will be, so just make a big ass buffer
	resp := make([]byte, 4096)
	n, err := u.conn.Read(resp)
	if err != nil {
		return fmt.Errorf("reading announce response: %w", err)
	}

	// adjust length of response buffer based on number of bytes read
	// workaround for not being able to read a UDP packet in pieces and not knowing its size upfront
	resp = resp[:n]

	if n < 20 {
		return fmt.Errorf("announce response must be at least 20 characters, got %d", n)
	}
	action := binary.BigEndian.Uint32(resp[0:4])
	if Action(action) != AnnounceAction {
		return fmt.Errorf("unexpected action on response from announce %v", action)
	}
	respTransactionID := binary.BigEndian.Uint32(resp[4:8])
	if transactionID != respTransactionID {
		return fmt.Errorf("transaction ids do not match in announce resp")
	}

	interval := binary.BigEndian.Uint32(resp[8:12])
	leecher := binary.BigEndian.Uint32(resp[12:16])
	seeders := binary.BigEndian.Uint32(resp[16:20])
	// currently unused statistics
	_, _, _ = interval, leecher, seeders

	var peers []net.TCPAddr
	for i := 20; i < len(resp); i += 6 {
		// parse 6 bytes for peer's ip (4 bytes) and port (2 bytes)
		peers = append(peers, net.TCPAddr{
			IP:   resp[i : i+4],
			Port: int(binary.BigEndian.Uint16(resp[i+4 : i+6])),
		})
	}

	if len(peers) == 0 {
		return fmt.Errorf("no peers found")
	}
	u.peers = peers

	return nil
}

// Scrape gets data on the torrent including seeders, completed and leechers.
// I'm not sure if it's necessary to use for a client... Once I get all peers
// from `announce` I feel like I'm good to go?
//
// This implementation is limited to scraping data of a SINGLE torrent
func (u *UDPTrackerClient) scrape() error {
	transactionID := rand.Uint32()
	scrapeMsg := make([]byte, 36)

	binary.BigEndian.PutUint64(scrapeMsg[0:8], u.connectionID)
	binary.BigEndian.PutUint32(scrapeMsg[8:12], uint32(ScrapeAction))
	binary.BigEndian.PutUint32(scrapeMsg[12:16], transactionID)

	copy(scrapeMsg[16:36], u.infoHash[:])
	_, err := u.conn.Write(scrapeMsg)
	if err != nil {
		return fmt.Errorf("writing scrape msg: %w", err)
	}

	// limiting to one torrent so msg should be 20 bytes
	respScrape := make([]byte, 20)
	n, err := u.conn.Read(respScrape)
	if err != nil {
		return fmt.Errorf("reading scrape msg: %w", err)
	}
	if n != 20 {
		return fmt.Errorf("expected 20 byte scrape response, got %d", n)
	}
	action := binary.BigEndian.Uint32(respScrape[0:4])
	if Action(action) != ScrapeAction {
		return fmt.Errorf("expected scrape action response, got %v", Action(action))
	}

	respTransactionID := binary.BigEndian.Uint32(respScrape[4:8])
	if respTransactionID != transactionID {
		return fmt.Errorf("transaction ids do not match")
	}

	seeders := binary.BigEndian.Uint32(respScrape[8:12])
	completed := binary.BigEndian.Uint32(respScrape[12:16])
	leechers := binary.BigEndian.Uint32(respScrape[16:20])

	// unused currently
	_, _, _ = seeders, completed, leechers

	return nil
}

// ParseErrorMsg currently receives the entire message byte slice
// TODO refactor into a ReadMsg type for all UDP Tracker Responses?
func (u *UDPTrackerClient) parseErrorMsg(msg []byte) (string, error) {
	if len(msg) < 8 {
		return "", fmt.Errorf("error message is <8 characters, got %d", len(msg))
	}
	action := binary.BigEndian.Uint32(msg[0:4])
	if Action(action) != ErrorAction {
		return "", fmt.Errorf("expected error action")
	}
	respTransactionID := binary.BigEndian.Uint32(msg[4:8])
	// todo compare to transactionID of original message
	_ = respTransactionID

	message := string(msg[8:])
	return message, nil
}
