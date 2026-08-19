package main

// Putting the caption bytes into the transport stream.
//
// ATSC A/53 user data inside an SEI, the SEI into the access unit, the access
// unit back into transport packets, and the program table taught to advertise
// the caption service. Nothing here re-encodes video.

import (
	"bytes"
	"io"
	"math"
	"sort"
	"time"
)

// ATSC A/53 caption user data and SEI
// ---------------------------------------------------------------------------

// buildCCData assembles the cc_data() structure from A/53 Part 4. ccCount is
// fixed by the frame rate: the caption channel runs at 9600 bits per second, so
// each frame carries 600/fps constructs of two bytes each.
func buildCCData(pair [2]byte, ccCount int, send608 bool) []byte {
	if ccCount < 2 {
		ccCount = 2
	}
	if ccCount > 31 {
		ccCount = 31
	}
	b := make([]byte, 0, 5+3*ccCount+1)
	b = append(b,
		0xB5,       // itu_t_t35_country_code, United States
		0x00, 0x31, // itu_t_t35_provider_code, ATSC
		'G', 'A', '9', '4', // user_identifier
		0x03, // user_data_type_code, cc_data
	)
	// process_em_data_flag=1, process_cc_data_flag=1, additional_data_flag=0
	b = append(b, 0xC0|byte(ccCount))
	b = append(b, 0xFF) // em_data

	for i := 0; i < ccCount; i++ {
		// marker_bits(5)=11111, cc_valid(1), cc_type(2)
		switch {
		case i == 0 && send608:
			// Field 1, which carries the captions.
			b = append(b, 0xFC, pair[0], pair[1])
		case i == 1 && send608:
			// Field 2, likewise. Claiming a field carries something it does not
			// makes a player offer a caption track with nothing in it.
			b = append(b, 0xF9, 0x00, 0x00)
		case i == 0 || i == 1:
			// This picture carries no line 21 data. Marked invalid rather than
			// omitted: the construct count is fixed by the picture rate and the
			// two line 21 slots are always the first two.
			b = append(b, byte(0xF8|i), 0x00, 0x00)
		default: // nothing to send this picture; padding, marked invalid
			b = append(b, 0xFA, 0x00, 0x00)
		}
	}
	b = append(b, 0xFF) // marker_bits
	return b
}

// emulationPrevention inserts the 0x03 escape bytes that keep a NAL payload
// from accidentally containing a start code.
func emulationPrevention(in []byte) []byte {
	out := make([]byte, 0, len(in)+len(in)/64+8)
	zeros := 0
	for _, b := range in {
		if zeros >= 2 && b <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, b)
		if b == 0x00 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// seiPayloadSize writes an SEI size using the spec's 255-at-a-time encoding.
func seiPayloadSize(n int) []byte {
	var out []byte
	for n >= 255 {
		out = append(out, 0xFF)
		n -= 255
	}
	return append(out, byte(n))
}

// buildCaptionSEI produces a complete Annex-B NAL, start code included, that
// carries the frame's caption bytes as registered ITU-T T.35 user data.
func buildCaptionSEI(pair [2]byte, ccCount int, send608 bool, hevc bool) []byte {
	payload := buildCCData(pair, ccCount, send608)

	rbsp := make([]byte, 0, len(payload)+8)
	rbsp = append(rbsp, 0x04) // payloadType 4, user_data_registered_itu_t_t35
	rbsp = append(rbsp, seiPayloadSize(len(payload))...)
	rbsp = append(rbsp, payload...)
	rbsp = append(rbsp, 0x80) // rbsp_trailing_bits

	nal := make([]byte, 0, len(rbsp)+6)
	nal = append(nal, 0x00, 0x00, 0x00, 0x01)
	if hevc {
		// nal_unit_type 39 (PREFIX_SEI_NUT), layer 0, temporal id 0.
		nal = append(nal, 0x4E, 0x01)
	} else {
		// nal_ref_idc 0, nal_unit_type 6 (SEI).
		nal = append(nal, 0x06)
	}
	return append(nal, emulationPrevention(rbsp)...)
}

// ---------------------------------------------------------------------------
// Transport stream parsing and injection
// ---------------------------------------------------------------------------

const tsPacketSize = 188

const (
	streamTypeH264 = 0x1B
	streamTypeHEVC = 0x24
)

// nalStarts finds every Annex-B start code offset in an elementary stream.
func nalStarts(es []byte) []int {
	var out []int
	for i := 0; i+3 < len(es); i++ {
		if es[i] == 0 && es[i+1] == 0 {
			if es[i+2] == 1 {
				out = append(out, i)
				i += 2
			} else if es[i+2] == 0 && i+3 < len(es) && es[i+3] == 1 {
				out = append(out, i)
				i += 3
			}
		}
	}
	return out
}

// nalHeaderAt returns the payload offset and NAL type for the start code at p.
func nalHeaderAt(es []byte, p int, hevc bool) (int, int, bool) {
	off := p + 3
	if es[p+2] == 0 {
		off = p + 4
	}
	if off >= len(es) {
		return 0, 0, false
	}
	if hevc {
		return off, int((es[off] >> 1) & 0x3F), true
	}
	return off, int(es[off] & 0x1F), true
}

// isVCL reports whether a NAL type carries coded picture data.
func isVCL(t int, hevc bool) bool {
	if hevc {
		return t <= 31
	}
	return t >= 1 && t <= 5
}

// injectSEI places a caption SEI immediately before the first coded slice of
// the access unit, which is where A/53 requires it and where every decoder
// looks. Returns the elementary stream unchanged if no slice is found.
func injectSEI(es []byte, sei []byte, hevc bool) []byte {
	for _, p := range nalStarts(es) {
		off, t, ok := nalHeaderAt(es, p, hevc)
		if !ok {
			continue
		}
		_ = off
		if isVCL(t, hevc) {
			out := make([]byte, 0, len(es)+len(sei))
			out = append(out, es[:p]...)
			out = append(out, sei...)
			out = append(out, es[p:]...)
			return out
		}
	}
	return es
}

// hasCaptionSEI reports whether the access unit already carries A/53 captions,
// in which case we leave the stream alone rather than fighting the source.
func hasCaptionSEI(es []byte, hevc bool) bool {
	for _, p := range nalStarts(es) {
		off, t, ok := nalHeaderAt(es, p, hevc)
		if !ok {
			continue
		}
		seiType := 6
		if hevc {
			seiType = 39
		}
		if t != seiType {
			continue
		}
		hdr := 1
		if hevc {
			hdr = 2
		}
		// A start code can sit close enough to the end that the NAL header runs
		// past it. That only happens on a stream truncated mid-packet, but it
		// is a slice out of range when it does.
		if off+hdr > len(es) {
			continue
		}
		body := es[off+hdr:]
		if len(body) > 12 && body[0] == 0x04 {
			for i := 1; i < len(body)-8 && i < 12; i++ {
				if body[i] == 0xB5 && body[i+1] == 0x00 && body[i+2] == 0x31 &&
					body[i+3] == 'G' && body[i+4] == 'A' && body[i+5] == '9' && body[i+6] == '4' {
					return true
				}
			}
		}
	}
	return false
}

// tsPacket is one 188 byte packet held while an access unit is assembled.
type tsPacket struct {
	buf   [tsPacketSize]byte
	video bool
	// payload is false for a packet that carries only an adaptation field.
	// Those hold the clock rather than the picture, and must be passed through
	// where they are rather than rebuilt or dropped.
	payload bool
}

// captionInjector rewrites a transport stream in place, adding caption bytes to
// each video access unit and passing every other packet through untouched.
type captionInjector struct {
	out      io.Writer
	enc      *cea608
	log      string
	videoPID int
	pmtPID   int
	hevc     bool

	window   []tsPacket // packets held for the access unit being assembled
	pes      []byte     // the video PES currently being reassembled
	inPES    bool
	videoCC  byte
	ccSeeded bool // whether videoCC has picked up the source's count

	carry []byte // bytes of a packet split across two Write calls
	// pmtPatch is the program table rewritten to announce the caption
	// service; pmtDone records that the attempt has been made.
	pmtPatch []byte
	pmtDone  bool

	ccCount  int
	lastPTS  int64
	haveRate bool
	// ccOwed is how much of a line 21 pair this picture is due, accumulated so
	// the pairs land at the rate the format runs at rather than at the rate
	// pictures happen to arrive. See sendsCC.
	ccOwed  float64
	ccPerAU float64
	ptsGaps []int64

	frames   int64
	injected int64
	warned   bool
}

func newCaptionInjector(out io.Writer, enc *cea608, label string) *captionInjector {
	return &captionInjector{
		out:      out,
		enc:      enc,
		log:      label,
		videoPID: -1,
		pmtPID:   -1,
		ccCount:  20,  // 29.97 fps until the stream tells us otherwise
		ccPerAU:  1.0, // and one pair per picture at that rate
	}
}

// Write consumes transport stream bytes.
//
// Reads off a socket land wherever they land, so a packet routinely straddles
// two calls. The tail of a short call is carried over and completed by the next
// one; letting a split packet through unparsed would leave it out of the access
// unit while its bytes still reached the output, which corrupts the picture.
func (ci *captionInjector) Write(b []byte) (int, error) {
	consumed := len(b)

	if len(ci.carry) > 0 {
		need := tsPacketSize - len(ci.carry)
		if len(b) < need {
			ci.carry = append(ci.carry, b...)
			return consumed, nil
		}
		ci.carry = append(ci.carry, b[:need]...)
		b = b[need:]
		if err := ci.packet(ci.carry); err != nil {
			return consumed, err
		}
		ci.carry = ci.carry[:0]
	}

	for len(b) > 0 {
		if b[0] != 0x47 {
			// Resynchronize on the next sync byte rather than corrupting
			// output. A lone 0x47 is not enough: the byte 188 further on has to
			// be one too, or this locks onto a coincidence inside the picture
			// data and feeds it to the reassembler as if it were a packet.
			i := 1
			for i < len(b) {
				if b[i] == 0x47 && (i+tsPacketSize >= len(b) || b[i+tsPacketSize] == 0x47) {
					break
				}
				i++
			}
			if _, err := ci.out.Write(b[:i]); err != nil {
				return consumed, err
			}
			b = b[i:]
			continue
		}
		if len(b) < tsPacketSize {
			ci.carry = append(ci.carry[:0], b...)
			break
		}
		if err := ci.packet(b[:tsPacketSize]); err != nil {
			return consumed, err
		}
		b = b[tsPacketSize:]
	}
	return consumed, nil
}

// Flush emits whatever is still held for the access unit in flight. Without it
// the tail of the stream, including the audio packets interleaved into the last
// window, would be dropped when the source ends.
func (ci *captionInjector) Flush() error {
	if err := ci.passthroughWindow(); err != nil {
		return err
	}
	if len(ci.carry) > 0 {
		if _, err := ci.out.Write(ci.carry); err != nil {
			return err
		}
		ci.carry = ci.carry[:0]
	}
	return nil
}

func (ci *captionInjector) packet(p []byte) error {
	pid := int(p[1]&0x1F)<<8 | int(p[2])
	pusi := p[1]&0x40 != 0

	switch {
	case pid == 0:
		ci.parsePAT(p)
	case pid == ci.pmtPID:
		ci.parsePMT(p)
	}

	// Announce the caption service in the program table, so a player that
	// does not decode the video to look for caption messages still knows they
	// are there.
	if pid == ci.pmtPID && ci.videoPID >= 0 && !ci.pmtDone {
		if q := addCaptionDescriptor(p, ci.videoPID); q != nil {
			ci.pmtPatch = q
			logger("[CC] %s announced the caption service in the program table", ci.log)
		}
		ci.pmtDone = true
	}
	if pid == ci.pmtPID && ci.pmtPatch != nil {
		if ci.inPES {
			var t tsPacket
			copy(t.buf[:], ci.pmtPatch)
			ci.window = append(ci.window, t)
			return nil
		}
		_, err := ci.out.Write(ci.pmtPatch)
		return err
	}

	if ci.videoPID < 0 || pid != ci.videoPID {
		// Not video: hold it in the window so ordering survives, or write it
		// straight out when no access unit is in flight.
		if ci.inPES {
			var t tsPacket
			copy(t.buf[:], p)
			ci.window = append(ci.window, t)
			return nil
		}
		_, err := ci.out.Write(p)
		return err
	}

	// Video packets that arrived before the PMT identified the video PID went
	// out untouched, carrying the source's own count. Pick that count up so the
	// handover leaves no gap.
	//
	// Only a packet that carries payload may be used for it. The counter names
	// the value the next payload packet should take, and a packet without
	// payload repeats the previous one, so seeding from such a packet lands one
	// short and stamps a value that has already been used.
	if !ci.ccSeeded {
		if afc := (p[3] >> 4) & 0x03; afc != 0x01 && afc != 0x03 {
			_, err := ci.out.Write(p)
			return err
		}
		ci.videoCC = p[3] & 0x0F
		ci.ccSeeded = true
	}

	if pusi && ci.inPES {
		if err := ci.flush(); err != nil {
			return err
		}
	}
	if pusi {
		ci.inPES = true
		ci.pes = ci.pes[:0]
	}
	if !ci.inPES {
		ci.stampVideoCC(p)
		_, err := ci.out.Write(p)
		return err
	}

	var t tsPacket
	copy(t.buf[:], p)
	t.video = true
	if afc := (p[3] >> 4) & 0x03; afc == 0x01 || afc == 0x03 {
		t.payload = true
	}
	ci.window = append(ci.window, t)
	ci.pes = append(ci.pes, tsPayload(p)...)

	// A pathological stream with no further PUSI must not grow without bound.
	if len(ci.window) > 4096 {
		if !ci.warned {
			logger("[CC] %s access unit exceeded the reassembly limit, passing through", ci.log)
			ci.warned = true
		}
		return ci.passthroughWindow()
	}
	return nil
}

// tsPayload returns the payload bytes of a transport packet, skipping any
// adaptation field.
func tsPayload(p []byte) []byte {
	afc := (p[3] >> 4) & 0x03
	switch afc {
	case 0x01:
		return p[4:]
	case 0x03:
		l := int(p[4])
		if 5+l > tsPacketSize {
			return nil
		}
		return p[5+l:]
	}
	return nil
}

// mpegCRC is the CRC-32 the PSI tables carry, MSB first with no final inversion.
func mpegCRC(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// captionDescriptor announces the one service this stream carries.
//
// A player that does not decode the video looking for caption messages finds
// out captions exist from this and nothing else, which is why some show none
// without it. Announcing a service that turns out to be empty is the thing to
// avoid: a decoder told it is there and finding nothing shows a blank.
var captionDescriptor = []byte{
	0x86, 0x07, // tag, length
	0xE1, // reserved, one service

	'e', 'n', 'g', // language
	0x7F, // analogue service on field 1
	0x7F, // not easy reader, wide aspect
	0xFF, // reserved
}

// addCaptionDescriptor rewrites a PMT so the video stream announces a caption
// service. It returns nil when the table cannot be rewritten safely, in which
// case the original is passed through untouched.
func addCaptionDescriptor(p []byte, videoPID int) []byte {
	if p[1]&0x40 == 0 {
		return nil // not the start of a section
	}
	pl := tsPayload(p)
	off := len(p) - len(pl) // where the payload begins in the packet
	sec := psiSection(pl)
	if len(sec) < 16 || sec[0] != 0x02 {
		return nil
	}
	slen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + slen
	if end > len(sec) || slen < 13 {
		return nil
	}
	body := sec[:end-4] // section without its CRC

	il := int(body[10]&0x0F)<<8 | int(body[11])
	i := 12 + il
	out := append([]byte(nil), body[:i]...)
	found := false
	for i+4 < len(body) {
		st := int(body[i])
		pid := int(body[i+1]&0x1F)<<8 | int(body[i+2])
		esil := int(body[i+3]&0x0F)<<8 | int(body[i+4])
		if i+5+esil > len(body) {
			return nil
		}
		entry := append([]byte(nil), body[i:i+5+esil]...)
		if pid == videoPID && (st == streamTypeH264 || st == streamTypeHEVC) {
			if bytes.Contains(entry[5:], []byte{0x86}) {
				return nil // the source already announces captions
			}
			desc := captionDescriptor
			entry = append(entry, desc...)
			n := esil + len(desc)
			entry[3] = byte(n>>8) | 0xF0
			entry[4] = byte(n)
			found = true
		}
		out = append(out, entry...)
		i += 5 + esil
	}
	if !found {
		return nil
	}
	// Restate the section length, then the CRC over everything before it.
	n := len(out) + 4 - 3
	if n > 0x3FD {
		return nil
	}
	out[1] = byte(n>>8) | 0xB0
	out[2] = byte(n)
	crc := mpegCRC(out)
	out = append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	// Rebuild the packet: pointer field, section, then stuffing.
	if off+1+len(out) > tsPacketSize {
		return nil
	}
	var q [tsPacketSize]byte
	copy(q[:], p[:off])
	q[off] = 0x00 // pointer_field
	copy(q[off+1:], out)
	for j := off + 1 + len(out); j < tsPacketSize; j++ {
		q[j] = 0xFF
	}
	r := make([]byte, tsPacketSize)
	copy(r, q[:])
	return r
}

// psiSection skips the pointer_field at the head of a PSI payload.
//
// That field is a whole byte and can therefore claim up to 255, while the
// payload it indexes into is at most 184. A packet saying so is malformed, but
// malformed packets arrive: an encoder losing its HDMI signal emits rubbish,
// and the resync path can hand over a run of bytes that merely begins with the
// sync byte. Trusting it panicked, and a panic here takes the whole proxy down
// with every tuner on it, not just the captions.
func psiSection(pl []byte) []byte {
	if len(pl) < 1 {
		return nil
	}
	skip := 1 + int(pl[0])
	if skip > len(pl) {
		return nil
	}
	return pl[skip:]
}

func (ci *captionInjector) parsePAT(p []byte) {
	if p[1]&0x40 == 0 || ci.pmtPID >= 0 {
		return
	}
	pl := psiSection(tsPayload(p))
	if len(pl) < 12 || pl[0] != 0x00 {
		return
	}
	sl := int(pl[1]&0x0F)<<8 | int(pl[2])
	end := 3 + sl - 4
	if end > len(pl) {
		return
	}
	for i := 8; i+3 < end; i += 4 {
		prog := int(pl[i])<<8 | int(pl[i+1])
		pid := int(pl[i+2]&0x1F)<<8 | int(pl[i+3])
		if prog != 0 {
			ci.pmtPID = pid
			return
		}
	}
}

func (ci *captionInjector) parsePMT(p []byte) {
	if p[1]&0x40 == 0 || ci.videoPID >= 0 {
		return
	}
	pl := psiSection(tsPayload(p))
	if len(pl) < 16 || pl[0] != 0x02 {
		return
	}
	sl := int(pl[1]&0x0F)<<8 | int(pl[2])
	end := 3 + sl - 4
	if end > len(pl) {
		return
	}
	il := int(pl[10]&0x0F)<<8 | int(pl[11])
	i := 12 + il
	for i+4 < end {
		st := int(pl[i])
		pid := int(pl[i+1]&0x1F)<<8 | int(pl[i+2])
		esil := int(pl[i+3]&0x0F)<<8 | int(pl[i+4])
		switch st {
		case streamTypeH264:
			ci.videoPID, ci.hevc = pid, false
			logger("[CC] %s video is H.264 on PID %d", ci.log, pid)
		case streamTypeHEVC:
			ci.videoPID, ci.hevc = pid, true
			logger("[CC] %s video is HEVC on PID %d", ci.log, pid)
		}
		if ci.videoPID >= 0 {
			return
		}
		i += 5 + esil
	}
}

// stampVideoCC rewrites a video packet's continuity counter from our own
// sequence.
//
// Adding captions makes an access unit larger, so our packet count for the
// video PID drifts away from the source's. Once that has happened, a packet
// carrying the source's original counter reads as a lost packet to the
// demuxer, which throws away the picture around it. Every packet on the video
// PID therefore gets its counter from us, whether we rebuilt it or not.
func (ci *captionInjector) stampVideoCC(p []byte) {
	if afc := (p[3] >> 4) & 0x03; afc == 0x00 || afc == 0x02 {
		// A packet with no payload does not advance the count; it repeats the
		// one the last payload packet used. Giving it the next value instead
		// reads as a lost packet either side of it.
		p[3] = (p[3] &^ 0x0F) | ((ci.videoCC - 1) & 0x0F)
		return
	}
	p[3] = (p[3] &^ 0x0F) | (ci.videoCC & 0x0F)
	ci.videoCC = (ci.videoCC + 1) & 0x0F
}

// passthroughWindow writes the held packets out unmodified apart from the video
// continuity counter, then resets state.
func (ci *captionInjector) passthroughWindow() error {
	for i := range ci.window {
		if ci.window[i].video {
			ci.stampVideoCC(ci.window[i].buf[:])
		}
		if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
			return err
		}
	}
	ci.window = ci.window[:0]
	ci.pes = ci.pes[:0]
	ci.inPES = false
	return nil
}

// flush rebuilds the completed access unit with captions attached and writes
// the window out in its original packet order.
func (ci *captionInjector) flush() error {
	if len(ci.pes) == 0 {
		return ci.passthroughWindow()
	}
	es, hdrLen, ptsVal, ok := splitPES(ci.pes)
	if !ok || len(es) == 0 {
		return ci.passthroughWindow()
	}
	if hasCaptionSEI(es, ci.hevc) {
		// The source already carries captions. Do not add a second set.
		return ci.passthroughWindow()
	}
	ci.trackFrameRate(ptsVal)

	// Everything recognized before the first picture goes out belongs to audio
	// the viewer will never hear.
	//
	// The recognizer starts on the encoder's audio the moment the tuner opens.
	// The viewer's stream starts later — the app is still tuning, playback
	// detection is waiting for a keyframe that moves — so by the time there is
	// a picture to carry captions, seconds of transcript can have piled up
	// behind audio from before anything the viewer sees. Dropping that onto the
	// first pictures puts captions on screen ahead of the sound that produced
	// them, which reads as the transcript running early.
	//
	// It depends on how long the tune took, which is why it happens on some
	// tunes and not others: playback after a fifth of a second leaves nothing
	// queued, playback after five seconds leaves five seconds of it.
	//
	// The viewer's timeline starts here, so this is where the encoder starts.
	if ci.injected == 0 {
		ci.enc.reset()
	}
	send := ci.onPicture(ptsVal)
	pair := [2]byte{}
	if send {
		pair = ci.enc.next()
	}
	sei := buildCaptionSEI(pair, ci.ccCount, send, ci.hevc)
	newES := injectSEI(es, sei, ci.hevc)
	if len(newES) == len(es) {
		// No slice NAL found; leave this access unit alone.
		return ci.passthroughWindow()
	}
	ci.frames++
	ci.injected++

	newPES := rebuildPES(ci.pes, hdrLen, newES)
	pkts := ci.packetize(newPES)
	return ci.emit(pkts)
}

// splitPES separates the PES header from the elementary stream and returns the
// PTS when one is present.
func splitPES(pes []byte) (es []byte, hdrLen int, pts int64, ok bool) {
	if len(pes) < 9 || pes[0] != 0x00 || pes[1] != 0x00 || pes[2] != 0x01 {
		return nil, 0, 0, false
	}
	optLen := int(pes[8])
	start := 9 + optLen
	if start > len(pes) {
		return nil, 0, 0, false
	}
	pts = -1
	if pes[7]&0x80 != 0 && optLen >= 5 {
		p := pes[9:]
		pts = int64(p[0]&0x0E)<<29 | int64(p[1])<<22 | int64(p[2]&0xFE)<<14 | int64(p[3])<<7 | int64(p[4])>>1
	}
	return pes[start:], start, pts, true
}

// rebuildPES reassembles a PES packet around a modified elementary stream.
func rebuildPES(orig []byte, hdrLen int, es []byte) []byte {
	unbounded := orig[4] == 0 && orig[5] == 0
	out := make([]byte, 0, hdrLen+len(es))
	out = append(out, orig[:hdrLen]...)
	out = append(out, es...)
	n := len(out) - 6
	if unbounded || n > 0xFFFF {
		// Zero means "runs to the next start", which is legal for video and is
		// what an encoder emits for a picture larger than 64 KB. Keep whichever
		// form the source chose.
		out[4], out[5] = 0, 0
	} else {
		out[4], out[5] = byte(n>>8), byte(n)
	}
	return out
}

// trackFrameRate derives the caption construct count from the picture rate, so
// the caption channel runs at the 9600 bits per second the spec calls for.
func (ci *captionInjector) trackFrameRate(pts int64) {
	if pts < 0 {
		return
	}
	if ci.lastPTS > 0 && !ci.haveRate {
		// Gathered rather than taken from the first pair of timestamps.
		//
		// This still runs, and only for the construct count: that is a field in
		// every picture and it is fixed by the picture rate, so the rate has to
		// be estimated whatever the cadence is driven from. The cadence and the
		// clock are driven from the timestamps themselves; see onPicture.
		//
		// A presentation timestamp is not the picture's position in decode
		// order: with B-frames the values arrive out of order, so consecutive
		// deltas are irregular and some are negative. One sample was being
		// taken and latched for the life of the stream, which is a coin toss on
		// anything with B-frames in it.
		//
		// The middle value of a handful is the picture interval whatever the
		// coding order does around it, because the irregularity is symmetric —
		// the frames that come early are the frames that later come late.
		if d := pts - ci.lastPTS; d > 0 {
			ci.ptsGaps = append(ci.ptsGaps, d)
			if len(ci.ptsGaps) >= ccRateSamples {
				ci.setRate(90000.0 / float64(medianInt64(ci.ptsGaps)))
			}
		}
	}
}

// ccRateSamples is how many picture intervals are gathered before the rate is
// taken. Enough that a run of B-frames cannot own the middle of them, few
// enough that the figure arrives inside the first second at any rate worth
// having.
const ccRateSamples = 15

// setRate works the caption cadence out from the picture rate, for any picture
// rate rather than for the ones anybody thought of.
//
// Two numbers come out of it and both are arithmetic, not a table.
//
// The construct count is what the format asks for at this picture rate: the
// caption channel is a fixed number of constructs a second, so each picture
// carries that many divided by the picture rate. It is clamped rather than
// rejected, because a rate outside the clamp is a rate this still has to do
// something sensible at — rejecting it was leaving the cadence at one pair per
// picture, which is the very fault this exists to prevent, and it did it at
// exactly the rates nobody tests.
//
// The line 21 cadence is the format's rate over the picture rate, capped at one
// pair per picture because there is nowhere to put a second one. Below the
// format's rate that cap binds and the channel simply runs slower — a
// twenty-five picture stream carries twenty-five pairs a second and there is no
// arrangement of bytes that carries more.
func (ci *captionInjector) setRate(fps float64) {
	if fps <= 0 || math.IsInf(fps, 0) || math.IsNaN(fps) {
		return
	}
	n := int(math.Round(ccConstructsPerSec / fps))
	if n < 2 {
		n = 2
	}
	if n > 31 {
		n = 31
	}
	ci.ccCount = n
	ci.ccPerAU = cc608NominalRate / fps
	if ci.ccPerAU > 1 {
		ci.ccPerAU = 1
	}
	ci.haveRate = true
	// What the encoder pays attention to is the rate pairs actually leave at,
	// which is the format's rate rather than the picture rate.
	ci.enc.setPictureRate(fps * ci.ccPerAU)
	logger("[CC] %s picture rate is %.2f fps, cc_count %d, line 21 at %.2f pairs a second",
		ci.log, fps, n, fps*ci.ccPerAU)
}

// ccConstructsPerSec is the caption channel's size in cc_data constructs a
// second, which is what fixes the per-picture count at any picture rate. Six
// hundred: twenty constructs at 29.97 pictures, ten at 59.94, twenty-four at
// twenty-five, twelve at fifty — the published counts are this number divided
// by the picture rate, so the number is what is written down rather than the
// counts.
const ccConstructsPerSec = 600.0

// medianInt64 returns the middle value, and does not disturb the caller's
// slice order beyond what sorting a copy costs.
func medianInt64(v []int64) int64 {
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// sendsCC reports whether this picture carries a line 21 pair.
//
// Line 21 is a 29.97 pair per second channel and it does not become a 60 pair
// per second channel because the video is sixty pictures a second. A pair was
// being put in every picture, so a sixty picture stream carried the caption
// channel at twice its own rate — which decoders disagree about, and that
// disagreement is what made the same recording look and pace differently in two
// players. One consumed everything as it arrived; one clocked line 21 at the
// rate the format defines and could not.
//
// So the pairs are metered out at the format's rate. One per picture at 29.97,
// one in every two at 59.94, and the fraction carried between pictures so it
// does not drift. A picture with none due says so — the two line 21 slots are
// still in the construct, marked invalid, because the count is fixed by the
// picture rate and only the contents vary.
func (ci *captionInjector) sendsCC() bool {
	ci.ccOwed += ci.ccPerAU
	if ci.ccOwed < 1 {
		return false
	}
	ci.ccOwed -= 1
	return true
}

// ccMaxJump is the largest step in the stream's timestamps that is taken as
// time passing rather than as the stream having been cut. Ten seconds: longer
// than any picture interval and shorter than anything anybody would splice.
const ccMaxJump = 10 * time.Second

// onPicture takes the presentation timestamp of an access unit, moves the
// encoder's clock to it, and says whether this picture carries a line 21 pair.
//
// Both answers come from the timestamps and not from a picture rate, which is
// the whole point. A rate is an average and it only holds while it is constant:
// a player that transcodes or remuxes hands over whatever it produced, not a
// locked sixty a second, and the interval wanders. Everything derived from an
// assumed rate then wanders with it — the caption cadence over-sends on the
// fast stretches and starves on the slow ones, and the clock the dwells are
// measured against drifts against the video for as long as the stream runs.
//
// Elapsed presentation time answers both exactly. The pair is owed when a
// 29.97th of a second of video has gone by, whatever number of pictures that
// took, and the fraction is carried so it does not drift. A stream with no
// timestamps at all falls back to the picture rate, which is the best that can
// be done without them.
func (ci *captionInjector) onPicture(pts int64) bool {
	if pts < 0 {
		// No timestamp on this access unit, so the picture rate is all there
		// is. The clock still has to move: a stream that carried no timestamps
		// at all would otherwise freeze it, and a frozen clock is a caption
		// that never rolls and never swaps — the display stopping dead rather
		// than running late.
		if r := ci.enc.pairRate(); r > 0 {
			ci.enc.advanceStream(time.Duration(float64(time.Second) / r * ci.ccPerAU))
		}
		return ci.sendsCC()
	}
	if ci.lastPTS <= 0 {
		ci.lastPTS = pts
		return true
	}
	d := time.Duration(pts-ci.lastPTS) * time.Second / 90000
	ci.lastPTS = pts
	if d <= 0 || d > ccMaxJump {
		// Backwards, or a jump this big, is the stream being cut rather than
		// time passing — a timestamp wrap, a splice, a decoder restart. Resync
		// on the new value and carry nothing across the seam.
		return true
	}
	ci.enc.advanceStream(d)
	ci.ccOwed += d.Seconds() * cc608NominalRate
	if ci.ccOwed < 1 {
		return false
	}
	ci.ccOwed -= 1
	// Never more than one pair behind. A stretch of stream that arrived with a
	// long gap in its timestamps cannot be repaid by sending pairs faster than
	// the format carries them, so the debt is dropped rather than carried.
	if ci.ccOwed > 1 {
		ci.ccOwed = 1
	}
	return true
}

// packetize turns a PES packet back into transport packets on the video PID.
// Continuity counters are left blank and assigned by emit, which is the only
// place that knows the order packets actually leave in: the clock-bearing
// packets kept from the source are interleaved with these.
//
// carrying the original adaptation field of the first packet so the PCR and any
// random access indicator survive.
func (ci *captionInjector) packetize(pes []byte) [][tsPacketSize]byte {
	// The clock can sit on any packet of the access unit, not only the first.
	// Collect what each source packet carried, in order, and give the same to
	// the rebuilt packet in that position; anything beyond gets none. The PCR
	// shifts by at most a packet, which is a fraction of a millisecond, where
	// dropping it costs the receiver its clock.
	var afs [][]byte
	for i := range ci.window {
		if ci.window[i].video && ci.window[i].payload {
			afs = append(afs, meaningfulAF(ci.window[i].buf[:]))
		}
	}

	var out [][tsPacketSize]byte
	first := true
	for len(pes) > 0 {
		var af []byte
		if k := len(out); k < len(afs) {
			af = afs[k]
		}
		var pkt [tsPacketSize]byte
		pkt[0] = 0x47
		pkt[1] = byte(ci.videoPID >> 8)
		pkt[2] = byte(ci.videoPID)
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
		}

		body := 4
		useAF := len(af) > 0
		if useAF {
			// adaptation_field_control = 11
			pkt[3] = 0x30
			pkt[4] = byte(len(af))
			copy(pkt[5:], af)
			body = 5 + len(af)
		} else {
			pkt[3] = 0x10
		}
		space := tsPacketSize - body
		if len(pes) < space {
			// Pad the tail with an adaptation field so the packet stays 188
			// bytes without inventing elementary stream data.
			stuff := space - len(pes)
			if useAF {
				// Extend the existing adaptation field.
				pkt[4] = byte(int(pkt[4]) + stuff)
				copy(pkt[body+stuff:], pes)
				for i := body; i < body+stuff; i++ {
					pkt[i] = 0xFF
				}
			} else {
				pkt[3] = 0x30
				pkt[4] = byte(stuff - 1)
				if stuff >= 2 {
					pkt[5] = 0x00
					for i := 6; i < 4+stuff; i++ {
						pkt[i] = 0xFF
					}
				}
				copy(pkt[4+stuff:], pes)
			}
			out = append(out, pkt)
			break
		}
		copy(pkt[body:], pes[:space])
		pes = pes[space:]
		out = append(out, pkt)
		first = false
	}
	return out
}

// meaningfulAF returns a packet's adaptation field only when it carries
// something the receiver needs: the program clock, or a flag marking a
// discontinuity, a random access point or a splice. An adaptation field that is
// nothing but stuffing is dropped, since the repacketizer adds its own where it
// needs to pad.
func meaningfulAF(p []byte) []byte {
	af := adaptationField(p)
	if len(af) == 0 {
		return nil
	}
	// discontinuity, random access, ES priority, PCR, OPCR, splicing point.
	if af[0]&0xFC == 0 {
		return nil
	}
	return af
}

// adaptationField returns the meaningful part of a packet's adaptation field so
// the PCR and the random access indicator survive repacketization.
//
// The length is computed from the flags rather than by trimming trailing 0xFF,
// because a PCR or OPCR can legitimately end in 0xFF and trimming those bytes
// leaves a field that claims more than it carries, which shifts the payload and
// corrupts the picture.
func adaptationField(p []byte) []byte {
	if (p[3]>>4)&0x02 == 0 {
		return nil
	}
	l := int(p[4])
	if l == 0 || 5+l > tsPacketSize {
		return nil
	}
	af := p[5 : 5+l]
	flags := af[0]
	n := 1
	if flags&0x10 != 0 { // PCR
		n += 6
	}
	if flags&0x08 != 0 { // OPCR
		n += 6
	}
	if flags&0x04 != 0 { // splicing point
		n++
	}
	if flags&0x02 != 0 && n < len(af) { // transport private data
		n += 1 + int(af[n])
	}
	if flags&0x01 != 0 && n < len(af) { // adaptation field extension
		n += 1 + int(af[n])
	}
	if n > len(af) {
		return af
	}
	return af[:n]
}

// emit writes the rebuilt video packets back into the window's original slots
// so the interleaving with audio and PSI stays close to the source.
func (ci *captionInjector) emit(pkts [][tsPacketSize]byte) error {
	n := 0
	for i := range ci.window {
		if ci.window[i].video {
			// A video packet with no payload carries the program clock, not
			// the picture. Overwriting it with rebuilt payload, or skipping it
			// once the rebuilt packets run out, deletes a PCR the receiver
			// needs; on a constant rate mux that is most of them.
			if !ci.window[i].payload {
				ci.stampVideoCC(ci.window[i].buf[:])
				if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
					return err
				}
				continue
			}
			if n < len(pkts) {
				ci.stampVideoCC(pkts[n][:])
				if _, err := ci.out.Write(pkts[n][:]); err != nil {
					return err
				}
				n++
			}
			continue
		}
		if _, err := ci.out.Write(ci.window[i].buf[:]); err != nil {
			return err
		}
	}
	// Captions make the access unit slightly larger, so anything left over goes
	// out right after the window it belongs to.
	for ; n < len(pkts); n++ {
		ci.stampVideoCC(pkts[n][:])
		if _, err := ci.out.Write(pkts[n][:]); err != nil {
			return err
		}
	}
	ci.window = ci.window[:0]
	ci.pes = ci.pes[:0]
	ci.inPES = false
	return nil
}

// ---------------------------------------------------------------------------
