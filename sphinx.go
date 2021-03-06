package sphinx

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"sync"

	"github.com/aead/chacha20"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil"
)

const (
	// So, 256-bit EC curve pubkeys, 160-bit keys symmetric encryption,
	// 160-bit keys for HMAC, etc. Represented in bytes.
	securityParameter = 20

	// Default message size in bytes. This is probably *much* too big atm?
	messageSize = 0

	// The maximum path length. This should be set to an
	// estiamate of the upper limit of the diameter of the node graph.
	NumMaxHops = 20

	// The number of bytes produced by our CSPRG for the key stream
	// implementing our stream cipher to encrypt/decrypt the mix header. The
	// last 2 * securityParameter bytes are only used in order to generate/check
	// the MAC over the header.
	numStreamBytes = (2*NumMaxHops + 2) * securityParameter

	// Size in bytes of the shared secrets.
	sharedSecretSize = 32

	// Per-hop payload size
	HopPayloadSize = 20

	// Fixed size of the the routing info. This consists of a 20
	// byte address and a 20 byte HMAC for each hop of the route,
	// the first pair in cleartext and the following pairs
	// increasingly obfuscated. In case fewer than numMaxHops are
	// used, then the remainder is padded with null-bytes, also
	// obfuscated.
	routingInfoSize = 2 * NumMaxHops * securityParameter

	// Length of the keys used to generate cipher streams and
	// encrypt payloads. Since we use SHA256 to generate the keys,
	// the maximum length currently is 32 bytes.
	keyLen = 32
)

// MixHeader is the onion wrapped hop-to-hop routing information neccessary to
// propagate a message through the mix-net without intermediate nodes having
// knowledge of their position within the route, the source, the destination,
// and finally the identities of the past/future nodes in the route. At each hop
// the ephemeral key is used by the node to perform ECDH between itself and the
// source node. This derived secret key is used to check the MAC of the entire mix
// header, decrypt the next set of routing information, and re-randomize the
// ephemeral key for the next node in the path. This per-hop re-randomization
// allows us to only propgate a single group element through the onion route.
type MixHeader struct {
	Version      byte
	EphemeralKey *btcec.PublicKey
	RoutingInfo  [routingInfoSize]byte
	HeaderMAC    [securityParameter]byte
	HopPayload   [NumMaxHops * HopPayloadSize]byte
}

// NewMixHeader creates a new mix header which is capable of
// obliviously routing a message through the mix-net path outline by
// 'paymentPath'.  This function returns the created mix header along
// with a derived shared secret for each node in the path.
func NewMixHeader(paymentPath []*btcec.PublicKey, sessionKey *btcec.PrivateKey,
	rawHopPayloads [][]byte, assocData []byte) (*MixHeader,
	[][sharedSecretSize]byte, error) {

	// Each hop performs ECDH with our ephemeral key pair to arrive at a
	// shared secret. Additionally, each hop randomizes the group element
	// for the next hop by multiplying it by the blinding factor. This way
	// we only need to transmit a single group element, and hops can't link
	// a session back to us if they have several nodes in the path.
	numHops := len(paymentPath)
	hopEphemeralPubKeys := make([]*btcec.PublicKey, numHops)
	hopSharedSecrets := make([][sha256.Size]byte, numHops)
	hopBlindingFactors := make([][sha256.Size]byte, numHops)

	// Compute the triplet for the first hop outside of the main loop.
	// Within the loop each new triplet will be computed recursively based
	// off of the blinding factor of the last hop.
	hopEphemeralPubKeys[0] = sessionKey.PubKey()
	hopSharedSecrets[0] = generateSharedSecret(paymentPath[0], sessionKey)
	hopBlindingFactors[0] = computeBlindingFactor(hopEphemeralPubKeys[0], hopSharedSecrets[0][:])

	// Now recursively compute the ephemeral ECDH pub keys, the shared
	// secret, and blinding factor for each hop.
	for i := 1; i <= numHops-1; i++ {
		// a_{n} = a_{n-1} x c_{n-1} -> (Y_prev_pub_key x prevBlindingFactor)
		hopEphemeralPubKeys[i] = blindGroupElement(hopEphemeralPubKeys[i-1],
			hopBlindingFactors[i-1][:])

		// s_{n} = sha256( y_{n} x c_{n-1} ) ->
		// (Y_their_pub_key x x_our_priv) x all prev blinding factors
		yToX := blindGroupElement(paymentPath[i], sessionKey.D.Bytes())
		hopSharedSecrets[i] = sha256.Sum256(multiScalarMult(yToX, hopBlindingFactors[:i]).SerializeCompressed())

		// TODO(roasbeef): prob don't need to store all blinding factors, only the prev...
		// b_{n} = sha256(a_{n} || s_{n})
		hopBlindingFactors[i] = computeBlindingFactor(hopEphemeralPubKeys[i],
			hopSharedSecrets[i][:])

	}

	// Generate the padding, called "filler strings" in the paper.
	filler := generateHeaderPadding("rho", numHops, 2*securityParameter, hopSharedSecrets)
	hopFiller := generateHeaderPadding("gamma", numHops, HopPayloadSize, hopSharedSecrets)

	// Allocate and initialize fields to zero-filled slices
	var mixHeader [routingInfoSize]byte
	var hopPayloads [NumMaxHops * HopPayloadSize]byte

	// Same goes for the HMAC
	var nextHmac [20]byte
	nextAddress := bytes.Repeat([]byte{0x00}, 20)

	// Now we compute the routing information for each hop, along with a
	// MAC of the routing info using the shared key for that hop.
	for i := numHops - 1; i >= 0; i-- {
		// We'll derive the three keys we need for each hop in order
		// to: generate our stream cipher bytes for the
		// mixHeader+hopPayloads, and calculate the MAC over the entire
		// constructed packet.
		rhoKey := generateKey("rho", hopSharedSecrets[i])
		gammaKey := generateKey("gamma", hopSharedSecrets[i])
		muKey := generateKey("mu", hopSharedSecrets[i])

		// Shift and obfuscate routing info
		streamBytes := generateCipherStream(rhoKey, numStreamBytes)
		rightShift(mixHeader[:], 2*securityParameter)
		copy(mixHeader[:], nextAddress[:])
		copy(mixHeader[securityParameter:], nextHmac[:])
		xor(mixHeader[:], mixHeader[:], streamBytes[:routingInfoSize])

		// Shift and obfuscate per-hop payload
		rightShift(hopPayloads[:], HopPayloadSize)
		copy(hopPayloads[:], rawHopPayloads[i])
		hopStreamBytes := generateCipherStream(gammaKey, uint(len(hopPayloads)))
		xor(hopPayloads[:], hopPayloads[:], hopStreamBytes)

		// We need to overwrite these so every node generates a correct padding
		if i == numHops-1 {
			copy(mixHeader[len(mixHeader)-len(filler):], filler)
			copy(hopPayloads[len(hopPayloads)-len(hopFiller):], hopFiller)
		}

		// The packet for this hop consists of: mixHeader ||
		// hopPayloads. When calculating the MAC, we'll also include
		// the optional associated data which can allow higher level
		// applications to prevent replay attacks.
		packet := append(append(mixHeader[:], hopPayloads[:]...),
			assocData...)
		nextHmac = calcMac(muKey, packet)
		nextAddress = btcutil.Hash160(paymentPath[i].SerializeCompressed())
	}

	header := &MixHeader{
		Version:      0x01,
		EphemeralKey: hopEphemeralPubKeys[0],
		RoutingInfo:  mixHeader,
		HeaderMAC:    nextHmac,
		HopPayload:   hopPayloads,
	}
	return header, hopSharedSecrets, nil
}

// Shift the byte-slice by the given number of bytes to the right and
// 0-fill the resulting gap.
func rightShift(slice []byte, num int) {
	for i := len(slice) - num - 1; i >= 0; i-- {
		slice[num+i] = slice[i]
	}
	for i := 0; i < num; i++ {
		slice[i] = 0
	}
}

// generateHeaderPadding derives the bytes for padding the mix header
// to ensure it remains fixed sized throughout route transit. At each
// step, we add 'hopSize' padding of zeroes, concatenate it to the
// previous filler, then decrypt it (XOR) with the secret key of the
// current hop. When encrypting the mix header we essentially do the
// reverse of this operation: we "encrypt" the padding, and drop
// 'hopSize' number of zeroes. As nodes process the mix header they
// add the padding ('hopSize') in order to check the MAC and decrypt
// the next routing information eventually leaving only the original
// "filler" bytes produced by this function at the last hop. Using
// this methodology, the size of the field stays constant at each
// hop.
func generateHeaderPadding(key string, numHops int, hopSize int,
	sharedSecrets [][sharedSecretSize]byte) []byte {

	filler := make([]byte, (numHops-1)*hopSize)
	for i := 1; i < numHops; i++ {
		totalFillerSize := ((NumMaxHops - i) + 1) * hopSize

		streamKey := generateKey(key, sharedSecrets[i-1])
		streamBytes := generateCipherStream(streamKey, numStreamBytes)

		xor(filler, filler, streamBytes[totalFillerSize:totalFillerSize+i*hopSize])
	}
	return filler
}

// OnionPacket represents a forwarding message containing onion wrapped
// hop-to-hop routing information along with an onion encrypted payload message
// addressed to the final destination.
type OnionPacket struct {
	Header *MixHeader
}

// NewOnionPaccket generates the a mix header containing the neccessary onion
// routing information required to propagate the message through the mixnet,
// eventually reaching the final node specified by a zero identifier.
func NewOnionPacket(route []*btcec.PublicKey, sessionKey *btcec.PrivateKey,
	hopPayloads [][]byte, assocData []byte) (*OnionPacket, error) {

	// Compute the mix header, and shared secrets for each hop.
	mixHeader, _, err := NewMixHeader(route, sessionKey, hopPayloads, assocData)
	if err != nil {
		return nil, err
	}

	return &OnionPacket{Header: mixHeader}, nil
}

// Encode serializes the raw bytes of the onoin packet into the passed
// io.Writer. The form encoded within the passed io.Writer is suitable for
// either storing on disk, or sending over the network.
func (f *OnionPacket) Encode(w io.Writer) error {
	ephemeral := f.Header.EphemeralKey.SerializeCompressed()

	if _, err := w.Write([]byte{f.Header.Version}); err != nil {
		return err
	}

	if _, err := w.Write(ephemeral); err != nil {
		return err
	}

	if _, err := w.Write(f.Header.HeaderMAC[:]); err != nil {
		return err
	}

	if _, err := w.Write(f.Header.RoutingInfo[:]); err != nil {
		return err
	}

	if _, err := w.Write(f.Header.HopPayload[:]); err != nil {
		return err
	}

	return nil
}

// Decode fully populates the target ForwardingMessage from the raw bytes
// encoded within the io.Reader. In the case of any decoding errors, an error
// will be returned. If the method success, then the new OnionPacket is ready
// to be processed by an instance of SphinxNode.
func (f *OnionPacket) Decode(r io.Reader) error {
	var err error

	f.Header = &MixHeader{}
	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}
	f.Header.Version = buf[0]

	var ephemeral [33]byte
	if _, err := io.ReadFull(r, ephemeral[:]); err != nil {
		return err
	}
	f.Header.EphemeralKey, err = btcec.ParsePubKey(ephemeral[:], btcec.S256())
	if err != nil {
		return err
	}

	if _, err := io.ReadFull(r, f.Header.HeaderMAC[:]); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, f.Header.RoutingInfo[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, f.Header.HopPayload[:]); err != nil {
		return err
	}

	return nil
}

// calcMac calculates HMAC-SHA-256 over the message using the passed secret key
// as input to the HMAC.
func calcMac(key [keyLen]byte, msg []byte) [securityParameter]byte {
	hmac := hmac.New(sha256.New, key[:])
	hmac.Write(msg)
	h := hmac.Sum(nil)

	var mac [securityParameter]byte
	copy(mac[:], h[:securityParameter])

	return mac
}

// xor computes the byte wise XOR of a and b, storing the result in dst.
func xor(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}
	return n
}

// generateKey generates a new key for usage in Sphinx packet
// construction/processing based off of the denoted keyType. Within Sphinx
// various keys are used within the same onion packet for padding generation,
// MAC generation, and encryption/decryption.
func generateKey(keyType string, sharedKey [sharedSecretSize]byte) [keyLen]byte {
	mac := hmac.New(sha256.New, []byte(keyType))
	mac.Write(sharedKey[:])
	h := mac.Sum(nil)

	var key [keyLen]byte
	copy(key[:], h[:keyLen])

	return key
}

// generateCipherStream generates a stream of cryptographic psuedo-random bytes
// intended to be used to encrypt a message using a one-time-pad like
// construction.
func generateCipherStream(key [keyLen]byte, numBytes uint) []byte {
	var (
		nonce [8]byte
	)
	cipher, err := chacha20.NewCipher(nonce[:], key[:])
	if err != nil {
		panic(err)
	}
	output := make([]byte, numBytes)
	cipher.XORKeyStream(output, output)

	return output
}

// computeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey, hopSharedSecret []byte) [sha256.Size]byte {
	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash [sha256.Size]byte
	copy(hash[:], sha.Sum(nil))
	return hash
}

// blindGroupElement blinds the group element by performing scalar
// multiplication of the group element by blindingFactor: G x blindingFactor.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor []byte) *btcec.PublicKey {
	newX, newY := hopPubKey.Curve.ScalarMult(hopPubKey.X, hopPubKey.Y, blindingFactor[:])
	return &btcec.PublicKey{hopPubKey.Curve, newX, newY}
}

// generateSharedSecret generates the shared secret for a particular hop. The
// shared secret is generated by taking the group element contained in the
// mix-header, and performing an ECDH operation with the node's long term onion
// key. We then take the _entire_ point generated by the ECDH operation,
// serialize that using a compressed format, then feed the raw bytes through a
// single SHA256 invocation.  The resulting value is the shared secret.
func generateSharedSecret(pub *btcec.PublicKey, priv *btcec.PrivateKey) [32]byte {
	s := &btcec.PublicKey{}
	x, y := pub.Curve.ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	s.X = x
	s.Y = y

	return sha256.Sum256(s.SerializeCompressed())
}

// multiScalarMult computes the cumulative product of the blinding factors
// times the passed public key.
//
// TODO(roasbeef): optimize using totient?
func multiScalarMult(hopPubKey *btcec.PublicKey, blindingFactors [][sha256.Size]byte) *btcec.PublicKey {
	finalPubKey := hopPubKey

	for _, blindingFactor := range blindingFactors {
		finalPubKey = blindGroupElement(finalPubKey, blindingFactor[:])
	}

	return finalPubKey
}

// ProcessCode is an enum-like type which describes to the high-level package
// user which action should be taken after processing a Sphinx packet.
type ProcessCode int

const (
	// ExitNode indicates that the node which processed the Sphinx packet
	// is the destination hop in the route.
	ExitNode = iota

	// MoreHops indicates that there are additional hops left within the
	// route. Therefore the caller should forward the packet to the node
	// denoted as the "NextHop".
	MoreHops

	// Failure indicates that a failure occurred during packet processing.
	Failure
)

// String returns a human readable string for each of the ProcessCodes.
func (p ProcessCode) String() string {
	switch p {
	case ExitNode:
		return "ExitNode"
	case MoreHops:
		return "MoreHops"
	case Failure:
		return "Failure"
	default:
		return "Unknown"
	}
}

// ProcessedPacket encapsulates the resulting state generated after processing
// an OnionPacket. A processed packet communicates to the caller what action
// shuold be taken after processing.
type ProcessedPacket struct {
	// Action represents the action the caller should take after processing
	// the packet.
	Action ProcessCode

	// NextHop is the next hop in the route the caller should forward the
	// onion packet to.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	NextHop [securityParameter]byte

	// Packet is the resulting packet uncovered after processing the
	// original onion packet by stripping off a layer from mix-header and
	// message.
	//
	// NOTE: This field will only be populated iff the above Action is
	// MoreHops.
	Packet *OnionPacket

	// DestMsg is the final e2e message addressed to the final destination.
	//
	// NOTE: This field will only be populated iff the above Action is
	// ExitNode.
	DestMsg []byte

	// HopPayload is the payload destined for the current hop
	//
	// This field contains instructions for the current node,
	// e.g., how many coins to forward.
	HopPayload [HopPayloadSize]byte
}

// Router is an onion router within the Sphinx network. The router is capable
// of processing incoming Sphinx onion packets thereby "peeling" a layer off
// the onion encryption which the packet is wrapped with.
type Router struct {
	nodeID [securityParameter]byte

	// TODO(roasbeef): swap out with btcutil.AddressLightningKey maybe?
	nodeAddr *btcutil.AddressPubKeyHash
	onionKey *btcec.PrivateKey

	sync.RWMutex
	seenSecrets map[[sharedSecretSize]byte]struct{}
}

// NewRouter creates a new instance of a Sphinx onion Router given the node's
// currently advertised onion private key, and the target Bitcoin network.
func NewRouter(nodeKey *btcec.PrivateKey, net *chaincfg.Params) *Router {
	var nodeID [securityParameter]byte
	copy(nodeID[:], btcutil.Hash160(nodeKey.PubKey().SerializeCompressed()))

	// Safe to ignore the error here, nodeID is 20 bytes.
	nodeAddr, _ := btcutil.NewAddressPubKeyHash(nodeID[:], net)

	return &Router{
		nodeID:   nodeID,
		nodeAddr: nodeAddr,
		onionKey: &btcec.PrivateKey{
			PublicKey: ecdsa.PublicKey{
				Curve: btcec.S256(),
				X:     nodeKey.X,
				Y:     nodeKey.Y,
			},
			D: nodeKey.D,
		},
		// TODO(roasbeef): replace instead with bloom filter?
		// * https://moderncrypto.org/mail-archive/messaging/2015/001911.html
		seenSecrets: make(map[[sharedSecretSize]byte]struct{}),
	}
}

// ProcessOnionPacket processes an incoming onion packet which has been forward
// to the target Sphinx router. If the encoded ephemeral key isn't on the
// target Elliptic Curve, then the packet is rejected. Similarly, if the
// derived shared secret has been seen before the packet is rejected.  Finally
// if the MAC doesn't check the packet is again rejected.
//
// In the case of a successful packet processing, and ProcessedPacket struct is
// returned which houses the newly parsed packet, along with instructions on
// what to do next.
func (r *Router) ProcessOnionPacket(onionPkt *OnionPacket, assocData []byte) (*ProcessedPacket, error) {
	mixHeader := onionPkt.Header

	dhKey := mixHeader.EphemeralKey
	routeInfo := mixHeader.RoutingInfo
	headerMac := mixHeader.HeaderMAC
	var hopPayload [HopPayloadSize]byte

	// Ensure that the public key is on our curve.
	if !r.onionKey.Curve.IsOnCurve(dhKey.X, dhKey.Y) {
		return nil, fmt.Errorf("pubkey isn't on secp256k1 curve")
	}

	// Compute our shared secret.
	sharedSecret := generateSharedSecret(dhKey, r.onionKey)

	// In order to mitigate replay attacks, if we've seen this particular
	// shared secret before, cease processing and just drop this forwarding
	// message.
	r.RLock()
	if _, ok := r.seenSecrets[sharedSecret]; ok {
		r.RUnlock()
		return nil, ErrReplayedPacket
	}
	r.RUnlock()

	// Using the derived shared secret, ensure the integrity of the routing
	// information by checking the attached MAC without leaking timing
	// information.
	message := append(append(routeInfo[:], mixHeader.HopPayload[:]...), assocData...)
	calculatedMac := calcMac(generateKey("mu", sharedSecret), message)
	if !hmac.Equal(headerMac[:], calculatedMac[:]) {
		return nil, fmt.Errorf("MAC mismatch %x != %x, rejecting "+
			"forwarding message", headerMac, calculatedMac)
	}

	// The MAC checks out, mark this current shared secret as processed in
	// order to mitigate future replay attacks. We need to check to see if
	// we already know the secret again since a replay might have happened
	// while we were checking the MAC.
	r.Lock()
	if _, ok := r.seenSecrets[sharedSecret]; ok {
		r.RUnlock()
		return nil, ErrReplayedPacket
	}
	r.seenSecrets[sharedSecret] = struct{}{}
	r.Unlock()

	// Attach the padding zeroes in order to properly strip an encryption
	// layer off the routing info revealing the routing information for the
	// next hop.
	var hopInfo [numStreamBytes]byte
	streamBytes := generateCipherStream(generateKey("rho", sharedSecret), numStreamBytes)
	headerWithPadding := append(routeInfo[:], bytes.Repeat([]byte{0}, 2*securityParameter)...)
	xor(hopInfo[:], headerWithPadding, streamBytes)

	// Randomize the DH group element for the next hop using the
	// deterministic blinding factor.
	blindingFactor := computeBlindingFactor(dhKey, sharedSecret[:])
	nextDHKey := blindGroupElement(dhKey, blindingFactor[:])

	// Parse out the ID of the next node in the route.
	var nextHop [securityParameter]byte
	copy(nextHop[:], hopInfo[:securityParameter])

	// MAC and MixHeader for the next hop.
	var nextMac [securityParameter]byte
	copy(nextMac[:], hopInfo[securityParameter:securityParameter*2])
	var nextMixHeader [routingInfoSize]byte
	copy(nextMixHeader[:], hopInfo[securityParameter*2:])

	// We repeat a similar process as above: we pad out the hop payload
	// with the required number of zeroes, then re-generate the stream
	// cipher bytes so we can XOR over the entire payload to decrypt our
	// payload layer and re-randomize the remainders.
	paddingZeroes := bytes.Repeat([]byte{0x00}, HopPayloadSize)
	hopPayloadsWithPadding := append(mixHeader.HopPayload[:], paddingZeroes...)
	payloadCipherKey := generateKey("gamma", sharedSecret)
	hopStreamBytes := generateCipherStream(payloadCipherKey, uint(len(hopPayloadsWithPadding)))
	xor(hopPayloadsWithPadding, hopPayloadsWithPadding, hopStreamBytes)

	// We'll copy over the decrypted portion of the payload for this hop,
	// and then copy over the remainders which is still encrypted and
	// destined for the next node.
	copy(hopPayload[:], hopPayloadsWithPadding[:HopPayloadSize])
	var nextHopPayloads [NumMaxHops * HopPayloadSize]byte
	copy(nextHopPayloads[:], hopPayloadsWithPadding[HopPayloadSize:])

	nextFwdMsg := &OnionPacket{
		Header: &MixHeader{
			Version:      onionPkt.Header.Version,
			EphemeralKey: nextDHKey,
			RoutingInfo:  nextMixHeader,
			HeaderMAC:    nextMac,
			HopPayload:   nextHopPayloads,
		},
	}

	// By default we'll assume that there are additional hops in the route.
	// However if the uncovered 'nextMac' is all zeroes, then this
	// indicates that we're the final hop in the route.
	var action ProcessCode = MoreHops
	if bytes.Compare(bytes.Repeat([]byte{0x00}, 20), nextMac[:]) == 0 {
		action = ExitNode
	}

	return &ProcessedPacket{
		Action:     action,
		NextHop:    nextHop,
		Packet:     nextFwdMsg,
		HopPayload: hopPayload,
	}, nil
}
