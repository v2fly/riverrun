package riverrun

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"

	"github.com/v2fly/riverrun/common/ctstretch"
	"github.com/v2fly/riverrun/common/drbg"
	f "github.com/v2fly/riverrun/common/framing"
	"github.com/v2fly/riverrun/common/log"
)

const (
	PacketTypePayload = iota
)

// Implements the net.Conn interface
type Conn struct {
	// Embeds a net.Conn and inherits its members.
	net.Conn

	logger log.Logger

	bias    float64
	mss_max int
	mss_dev float64

	Encoder *riverrunEncoder
	Decoder *riverrunDecoder
}

func get_rng(seed *drbg.Seed) (*rand.Rand, error) {
	xdrbg, err := drbg.NewHashDrbg(seed)
	if err != nil {
		return nil, err
	}
	return rand.New(xdrbg), nil
}

func get_mss(seed *drbg.Seed) (int, error) {
	rng, err := get_rng(seed)
	if err != nil {
		return 0, err
	}
	return int(rng.Float64()*float64(800)) + 600, nil
}

func NewConn(conn net.Conn, isServer bool, seed *drbg.Seed, logger log.Logger) (*Conn, error) {

	rng, err := get_rng(seed)

	key := make([]byte, 16)
	rng.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// We select the minimal expansion factors
	// The full range is commented out
	compressedBlockBits := uint64(16) // uint64((rng.Intn(2) + 1) * 8)

	var expandedBlockBits uint64
	var expandedBlockBits8 uint64
	if compressedBlockBits == 8 {
		expandedBlockBits = uint64((rng.Intn(6) + 3) * 8)
		expandedBlockBits8 = expandedBlockBits
	} else {
		expandedBlockBits = 32 // uint64((rng.Intn(3) + 2) * 16)
		expandedBlockBits8 = expandedBlockBits / 2
	}

	bias := rng.Float64()*.2 + .1 // Targeting entropy of 4-7 based on observations

	logger.Infof("rr: Set bias to %f, compressed block bits to %d, expanded block bits to %d", bias, compressedBlockBits, expandedBlockBits)

	iv := make([]byte, block.BlockSize())
	rng.Read(iv)
	table8, table16, err := getTables(expandedBlockBits8, expandedBlockBits, bias, key, block, iv, logger)
	if err != nil {
		return nil, err
	}

	var readStream, writeStream cipher.Stream
	rng.Read(iv)
	stream := cipher.NewCTR(block, iv)
	readKey := make([]byte, drbg.SeedLength)
	writeKey := make([]byte, drbg.SeedLength)
	logger.Debugf("riverrun: r/w keys made")

	if isServer {
		readStream = stream
		rng.Read(iv)
		writeStream = cipher.NewCTR(block, iv)
		rng.Read(readKey)
		rng.Read(writeKey)
	} else {
		writeStream = stream
		rng.Read(iv)
		readStream = cipher.NewCTR(block, iv)
		rng.Read(writeKey)
		rng.Read(readKey)
	}
	logger.Debugf("riverrun: Loaded keys properly")
	rr := new(Conn)
	rr.Conn = conn
	rr.logger = logger
	rr.bias = bias
	rr.mss_max, err = get_mss(seed)
	if err != nil {
		return nil, err
	}
	rr.mss_dev = rng.Float64() * 4
	logger.Infof("Set mss_max to %v, mss_dev to %v", rr.mss_max, rr.mss_dev)
	// Encoder
	rr.Encoder = newRiverrunEncoder(writeKey, writeStream, table8, table16, compressedBlockBits, expandedBlockBits, logger)
	logger.Debugf("riverrun: Encoder initialized")
	// Decoder
	rr.Decoder = newRiverrunDecoder(readKey, readStream, ctstretch.InvertTable(table8), ctstretch.InvertTable(table16), compressedBlockBits, expandedBlockBits, logger)
	logger.Debugf("riverrun: Initialized")
	return rr, nil
}

var cache8 map[string][]uint64
var cache16 map[string][]uint64
var mutex = &sync.Mutex{}

func getTables(expandedBlockBits8 uint64, expandedBlockBits uint64, bias float64, key []byte, block cipher.Block, iv []byte, logger log.Logger) ([]uint64, []uint64, error) {

	mutex.Lock()
	if cache8 == nil {
		cache8 = make(map[string][]uint64)
	}
	if cache16 == nil {
		cache16 = make(map[string][]uint64)
	}
	mutex.Unlock()

	mutex.Lock()
	table8, ok := cache8[string(key)]
	mutex.Unlock()
	if ok {
		mutex.Lock()
		table16, ok := cache16[string(key)]
		mutex.Unlock()
		if ok {
			logger.Debugf("riverrun: using cached tables")
			return table8, table16, nil
		}
	}

	logger.Debugf("riverrun: Generating fresh tables")
	stream := cipher.NewCTR(block, iv)

	table8, err := ctstretch.SampleBiasedStrings(expandedBlockBits8, 256, bias, stream)
	if err != nil {
		return nil, nil, err
	}
	logger.Debugf("riverrun: table8 prepped")
	table16, err := ctstretch.SampleBiasedStrings(expandedBlockBits, 65536, bias, stream)
	if err != nil {
		return nil, nil, err
	}
	logger.Debugf("riverrun: table16 prepped")

	mutex.Lock()
	cache8[string(key)] = table8
	cache16[string(key)] = table16
	mutex.Unlock()

	return table8, table16, nil
}

type riverrunEncoder struct {
	f.BaseEncoder

	logger log.Logger

	writeStream cipher.Stream

	table8  []uint64
	table16 []uint64

	compressedBlockBits uint64
	expandedBlockBits   uint64
}

func (encoder *riverrunEncoder) payloadOverhead(payloadLen int) int {
	return int(ctstretch.ExpandedNBytes(uint64(payloadLen), encoder.compressedBlockBits, encoder.expandedBlockBits)) - payloadLen
}
func (decoder *riverrunDecoder) payloadOverhead(payloadLen int) int {
	return int(ctstretch.ExpandedNBytes(uint64(payloadLen), decoder.compressedBlockBits, decoder.expandedBlockBits)) - payloadLen
}

func newRiverrunEncoder(key []byte, writeStream cipher.Stream, table8, table16 []uint64, compressedBlockBits, expandedBlockBits uint64, logger log.Logger) *riverrunEncoder {
	encoder := new(riverrunEncoder)
	encoder.logger = logger

	encoder.Drbg = f.GenDrbg(key[:])
	encoder.MaxPacketPayloadLength = int(ctstretch.CompressedNBytes_floor(f.MaximumSegmentLength-ctstretch.ExpandedNBytes(uint64(f.LengthLength), compressedBlockBits, expandedBlockBits), expandedBlockBits, compressedBlockBits))
	encoder.LengthLength = int(ctstretch.ExpandedNBytes(uint64(f.LengthLength), compressedBlockBits, expandedBlockBits))
	encoder.PayloadOverhead = encoder.payloadOverhead

	encoder.Encode = encoder.encode
	encoder.ProcessLength = encoder.processLength
	encoder.ChopPayload = encoder.makePayload

	encoder.writeStream = writeStream
	encoder.table8 = table8
	encoder.table16 = table16
	encoder.compressedBlockBits = compressedBlockBits
	encoder.expandedBlockBits = expandedBlockBits

	encoder.Type = "rr"

	return encoder
}

func (encoder *riverrunEncoder) processLength(length uint16) ([]byte, error) {
	lengthBytes := make([]byte, f.LengthLength)
	binary.BigEndian.PutUint16(lengthBytes[:], length)
	lengthBytesEncoded := make([]byte, encoder.LengthLength)
	err := ctstretch.ExpandBytes(lengthBytes[:], lengthBytesEncoded, encoder.compressedBlockBits, encoder.expandedBlockBits, encoder.table16, encoder.table8, encoder.writeStream, rand.Int(), encoder.logger)
	return lengthBytesEncoded, err
}

func (encoder *riverrunEncoder) encode(frame, payload []byte) (n int, err error) {
	tb := rand.Int()
	expandedNBytes := int(ctstretch.ExpandedNBytes(uint64(len(payload)), encoder.compressedBlockBits, encoder.expandedBlockBits))
	frameLen := encoder.LengthLength + expandedNBytes
	encoder.logger.Debugf("Encoding frame of length %d, with payload of length %d. TB: %d", frameLen, expandedNBytes, tb)
	err = ctstretch.ExpandBytes(payload[:], frame, encoder.compressedBlockBits, encoder.expandedBlockBits, encoder.table16, encoder.table8, encoder.writeStream, tb, encoder.logger)
	if err != nil {
		return 0, err
	}
	return expandedNBytes, err
}
func (encoder *riverrunEncoder) makePayload(pktType uint8, payload []byte) []byte {
	if pktType != PacketTypePayload {
		panic(fmt.Sprintf("BUG: pktType was not packetTypePayload for Riverrun"))
	}
	return payload[:]
}

type riverrunDecoder struct {
	f.BaseDecoder

	readStream cipher.Stream

	revTable8  map[uint64]uint64
	revTable16 map[uint64]uint64

	compressedBlockBits uint64
	expandedBlockBits   uint64

	logger log.Logger
}

func newRiverrunDecoder(key []byte, readStream cipher.Stream, revTable8, revTable16 map[uint64]uint64, compressedBlockBits, expandedBlockBits uint64, logger log.Logger) *riverrunDecoder {
	decoder := new(riverrunDecoder)
	decoder.logger = logger
	decoder.BaseDecoder.SetLogger(logger)

	decoder.Drbg = f.GenDrbg(key[:])
	decoder.LengthLength = int(ctstretch.ExpandedNBytes(uint64(f.LengthLength), compressedBlockBits, expandedBlockBits))
	decoder.MinPayloadLength = int(ctstretch.ExpandedNBytes(uint64(1), compressedBlockBits, expandedBlockBits))
	decoder.PacketOverhead = 0 // f.LengthLength
	decoder.MaxFramePayloadLength = f.MaximumSegmentLength - decoder.LengthLength

	// NextLength is set programatically
	// NextLengthInvalid is set programatically

	decoder.PayloadOverhead = decoder.payloadOverhead

	decoder.DecodeLength = decoder.decodeLength
	decoder.DecodePayload = decoder.decodePayload
	decoder.ParsePacket = decoder.parsePacket
	decoder.Cleanup = decoder.cleanup

	decoder.InitBuffers()

	decoder.readStream = readStream
	decoder.revTable8 = revTable8
	decoder.revTable16 = revTable16
	decoder.compressedBlockBits = compressedBlockBits
	decoder.expandedBlockBits = expandedBlockBits

	return decoder
}

func (decoder *riverrunDecoder) cleanup() error {
	return nil
}

func (decoder *riverrunDecoder) decodeLength(lengthBytes []byte) (uint16, error) {
	var decodedBytes [f.LengthLength]byte
	err := decoder.compressBytes(lengthBytes[:decoder.LengthLength], decodedBytes[:])
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(decodedBytes[:f.LengthLength]), err
}

func (decoder *riverrunDecoder) parsePacket(decoded []byte, decLen int) error {
	/*
		originalNBytes := binary.BigEndian.Uint16(decoded[:f.LengthLength]) // TODO: Ensure this is encoded
		if int(originalNBytes) > decLen-decoder.PacketOverhead {
			return f.InvalidPayloadLengthError(int(originalNBytes))
		}
	*/
	decoder.ReceiveDecodedBuffer.Write(decoded[decoder.PacketOverhead:decLen])
	return nil
}

func (decoder *riverrunDecoder) decodePayload(frames *bytes.Buffer) ([]byte, error) {
	//var frame []byte
	//var frameLen int
	frameLen, frame, err := decoder.GetFrame(frames)
	if err != nil {
		return nil, err
	}

	compressedNBytes := ctstretch.CompressedNBytes(uint64(frameLen), decoder.expandedBlockBits, decoder.compressedBlockBits)
	decodedPayload := make([]byte, compressedNBytes)
	err = decoder.compressBytes(frame[:frameLen], decodedPayload[:compressedNBytes])
	if err != nil {
		decoder.logger.Debugf("Max payload length is %d", int(ctstretch.CompressedNBytes_floor(f.MaximumSegmentLength-ctstretch.ExpandedNBytes(uint64(f.LengthLength), decoder.compressedBlockBits, decoder.expandedBlockBits), decoder.expandedBlockBits, decoder.compressedBlockBits)))
		decoder.logger.Debugf("CompressedNBytes: %d", compressedNBytes)
		decoder.logger.Debugf("Got payload of len %d", frameLen)
		return nil, err
	}

	return decodedPayload[:], nil
}

func (decoder *riverrunDecoder) compressBytes(raw, res []byte) error {
	return ctstretch.CompressBytes(raw, res, decoder.expandedBlockBits, decoder.compressedBlockBits, decoder.revTable16, decoder.revTable8, decoder.readStream, rand.Int(), decoder.logger)
}

func (rr *Conn) nextLength() int {
	noise := rand.NormFloat64() * rr.mss_dev
	if noise < 0 {
		noise = noise * -1
	}
	if int(noise) >= rr.mss_max {
		return rr.nextLength()
	}
	return rr.mss_max - int(noise)
}

func (rr *Conn) Write(b []byte) (n int, err error) {

	// XXX: n could be more accurate
	var frameBuf bytes.Buffer
	frameBuf, n, err = rr.Encoder.Chop(b, PacketTypePayload)
	if err != nil {
		return
	}

	// We do obfuscation here - experimental results found the
	//	constant near MSS sizes were detectable
	for {
		nextLength := rr.nextLength()
		toWire := make([]byte, nextLength)

		s, e := frameBuf.Read(toWire)
		if e != nil {
			if e != io.EOF {
				err = e
			}
			return
		}

		rr.logger.Debugf("Next length: %v", s)

		_, err = rr.Conn.Write(toWire[:s])
		if err != nil {
			return
		}
	}

	//log.Debugf("Riverrun: %d expanded to %d ->", n, lowerConnN)
	// TODO: What does spec say about returned numbers?
	//	 Should they be bytes written, or the raw bytes before expansion expanded?
	// Idea: Bytes written (raw), Bytes written (processed), err - raw bytes is equivalent to old n
}

func (rr *Conn) Read(b []byte) (int, error) {
	//originalLen := len(b)
	n, err := rr.Decoder.Read(b, rr.Conn)
	//log.Debugf("Riverrun: %d compressed to %d <-", originalLen, n)
	return n, err
}
