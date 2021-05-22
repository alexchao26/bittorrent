// Package tracker gets peers from (HTTP/S and UDP) tracker servers.
//
// Tracker servers are typically the primary method of peer discovery.
//
// Basic Usage
//
// The Tracker URL string can represent either a HTTP/S or UDP tracker address.
// tracker.GetPeers() will handle HTTP/S and UDP URLs separately.
//
//   // http
//   peerAddrs, err := tracker.GetPeers("http://academictorrents.com/announce.php")
//
//   // udp
//   peerAddrs, err := tracker.GetPeers("udp://tracker.opentrackr.org:1337/announce")
//
// The UDP Client implementation is exported as well, but does not need to be
// used in most use cases.
package tracker
