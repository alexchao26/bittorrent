package tracker

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"time"
)

func getPeersFromUDPTracker(u *url.URL, infoHash, peerID [20]byte, port int) ([]net.TCPAddr, error) {
	udpClient, err := NewUDPClient(u, infoHash, peerID, port)
	if err != nil {
		return nil, err
	}
	return udpClient.GetPeers()
}

// udpMessageAction is sent in Big Endian (network-byte order) on messages to
// and from a UDP Tracker.
type udpMessageAction uint32

const (
	ConnectAction udpMessageAction = iota
	AnnounceAction
	ScrapeAction
	ErrorAction
)

var actionStrings = map[udpMessageAction]string{
	ConnectAction:  "connect",
	AnnounceAction: "announce",
	ScrapeAction:   "scrape",
	ErrorAction:    "error",
}

func (m udpMessageAction) String() string {
	return actionStrings[m]
}

// UDPClient is an implementation of BEP0015 to locate peers without DHT.
// http://bittorrent.org/beps/bep_0015.html
type UDPClient struct {
	conn         *net.UDPConn
	peerID       [20]byte
	infoHash     [20]byte
	port         int
	peers        []net.TCPAddr
	connectionID uint64
}

// NewUDPClient generates a client to a UDP Tracker server per BEP0015.
func NewUDPClient(trURL *url.URL, infoHash, peerID [20]byte, port int) (*UDPClient, error) {
	// conn, err := net.DialTimeout(trURL.Scheme, trURL.Host, time.Second*5) // todo(alex) set timeout?
	udpAddr, err := net.ResolveUDPAddr(trURL.Scheme, trURL.Host)
	if err != nil {
		return nil, fmt.Errorf("resolving udp addr: %w", err)
	}
	udpConn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	err = udpConn.SetReadBuffer(4096)
	if err != nil {
		return nil, fmt.Errorf("setting udp conn read buffer: %w", err)
	}

	return &UDPClient{
		conn:     udpConn,
		peerID:   peerID,
		infoHash: infoHash,
		port:     port,
	}, nil
}

// GetPeers connects and announces to the UDP tracker, then returns the peer addresses
func (u *UDPClient) GetPeers() ([]net.TCPAddr, error) {
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

// Connect is the first message sent to a UDP Tracker Server to acquire a
// Connection ID to use for future requests (namely Announce)
func (u *UDPClient) connect() error {
	const protocolID = 0x41727101980 // magic constant
	transactionID := rand.Uint32()

	connectMsg := make([]byte, 16)
	binary.BigEndian.PutUint64(connectMsg[0:8], uint64(protocolID))
	binary.BigEndian.PutUint32(connectMsg[8:12], uint32(ConnectAction))
	binary.BigEndian.PutUint32(connectMsg[12:16], transactionID)

	u.conn.SetDeadline(time.Now().Add(time.Second * 3))
	defer u.conn.SetDeadline(time.Time{}) // clear deadlines

	_, err := u.conn.Write(connectMsg)
	if err != nil {
		return fmt.Errorf("sending connect msg to udp tracker: %w", err)
	}

	// Read connect response from server
	resp := make([]byte, 16)
	n, _, _, _, err := u.conn.ReadMsgUDP(resp, nil)
	if err != nil {
		return fmt.Errorf("reading connect resp: %w", err)
	}
	if n != 16 {
		return fmt.Errorf("want connect message to be 16 bytes, got %d", n)
	}
	connectResp, err := u.parseUDPResponse(transactionID, ConnectAction, resp)
	if err != nil {
		return fmt.Errorf("udp connect resp: %w", err)
	}

	// store connection id on client
	u.connectionID = binary.BigEndian.Uint64(connectResp)

	return nil
}

func (u *UDPClient) announce() error {
	announceMsg := make([]byte, 98)

	binary.BigEndian.PutUint64(announceMsg[0:8], u.connectionID)
	binary.BigEndian.PutUint32(announceMsg[8:12], uint32(AnnounceAction))
	transactionID := rand.Uint32()
	binary.BigEndian.PutUint32(announceMsg[12:16], transactionID)
	copy(announceMsg[16:36], u.infoHash[:])
	copy(announceMsg[36:56], u.peerID[:])

	binary.BigEndian.PutUint64(announceMsg[56:64], 0) // downloaded
	binary.BigEndian.PutUint64(announceMsg[64:72], 0) // left, unknown w/ magnet links
	binary.BigEndian.PutUint64(announceMsg[72:80], 0) // uploaded

	binary.BigEndian.PutUint32(announceMsg[80:84], 0) // event 0:none; 1:completed; 2:started; 3:stopped
	binary.BigEndian.PutUint32(announceMsg[84:88], 0) // IP address, default: 0

	binary.BigEndian.PutUint32(announceMsg[88:92], rand.Uint32()) // key - for tracker's statistics

	// trick go into allowing a negative unsigned int, it underflows
	neg1 := -1
	binary.BigEndian.PutUint32(announceMsg[92:96], uint32(neg1))   // num_want -1 default
	binary.BigEndian.PutUint16(announceMsg[96:98], uint16(u.port)) // port

	u.conn.SetDeadline(time.Now().Add(time.Second * 5))
	defer u.conn.SetDeadline(time.Time{}) // clear deadlines

	_, err := u.conn.Write(announceMsg)
	if err != nil {
		return fmt.Errorf("writing announce msg: %w", err)
	}

	// don't know upfront how many bytes a UDP message will be, so just allocate a big enough buffer
	// and resize it after
	resp := make([]byte, 4096)
	n, err := u.conn.Read(resp)
	if err != nil {
		return fmt.Errorf("reading announce response: %w", err)
	}
	resp = resp[:n]
	announceResp, err := u.parseUDPResponse(transactionID, AnnounceAction, resp)
	if err != nil {
		return fmt.Errorf("udp announce resp: %w", err)
	}

	interval := binary.BigEndian.Uint32(announceResp[0:4])
	leecher := binary.BigEndian.Uint32(announceResp[4:8])
	seeders := binary.BigEndian.Uint32(announceResp[8:12])
	// currently unused statistics
	_, _, _ = interval, leecher, seeders

	var peers []net.TCPAddr
	for i := 12; i < len(announceResp); i += 6 {
		// parse 6 bytes for peer's ip (4 bytes) and port (2 bytes)
		peers = append(peers, net.TCPAddr{
			IP:   announceResp[i : i+4],
			Port: int(binary.BigEndian.Uint16(announceResp[i+4 : i+6])),
		})
	}

	if len(peers) == 0 {
		return fmt.Errorf("no peers found")
	}
	u.peers = peers

	return nil
}

// scrape gets data on the torrent including seeders, completed and leechers.
//
// This implementation is limited to scraping data of a SINGLE torrent
//
// Currently scrape is unused because a bittorrent client doesn't need to get
// any additional stats once it gets peer addresses from the Announce message.
//lint:ignore U1000 unused but included for completion
func (u *UDPClient) scrape() error {
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
	_, err = u.conn.Read(respScrape)
	if err != nil {
		return fmt.Errorf("reading scrape msg: %w", err)
	}
	resp, err := u.parseUDPResponse(transactionID, ScrapeAction, respScrape)
	if err != nil {
		return fmt.Errorf("udp scrape resp: %w", err)
	}

	seeders := binary.BigEndian.Uint32(resp[0:4])
	completed := binary.BigEndian.Uint32(resp[4:8])
	leechers := binary.BigEndian.Uint32(resp[8:12])

	// unimplemented
	_, _, _ = seeders, completed, leechers

	return nil
}

// parseUDPResponse is a helper function that checks if the response has a
// matching transactionID and action type.
//
// In the event of an 'error' response, it includes the error message in the
// returned error.
//
// If there is no error it returns the rest of the response (index 8 to the end)
// for the caller to handle.
func (u *UDPClient) parseUDPResponse(wantTransactionID uint32, wantAction udpMessageAction, resp []byte) ([]byte, error) {
	if len(resp) < 8 {
		return nil, fmt.Errorf("response is <8 characters, got %d", len(resp))
	}

	respTransactionID := binary.BigEndian.Uint32(resp[4:8])
	if respTransactionID != wantTransactionID {
		return nil, fmt.Errorf("transactionIDs do not match, want %d, got %d", wantTransactionID, respTransactionID)
	}

	action := binary.BigEndian.Uint32(resp[0:4])
	if udpMessageAction(action) == ErrorAction {
		// return an error that includes the message
		errorText := string(resp[8:])
		return nil, fmt.Errorf("error response: %s", errorText)
	}
	if udpMessageAction(action) != wantAction {
		return nil, fmt.Errorf("want %s action, got %s", wantAction, udpMessageAction(action))
	}

	return resp[8:], nil
}
