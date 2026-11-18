package mpegts_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/asticode/go-astits"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/substructs"
)

const (
	audioTestPID = 123

	// duration of an AAC access unit at 48kHz in 90kHz clock units
	aacFrameDuration48k = mpeg4audio.SamplesPerAccessUnit * 90000 / 48000 // 1920

	// duration of an Opus packet (960 samples at 48kHz) in 90kHz clock units
	opusFrameDuration = 960 * 90000 / 48000 // 1800

	ts33Mask = int64(0x1FFFFFFFF)
)

// marshalOpusAccessUnits serializes packets into one opus PES payload.
func marshalOpusAccessUnits(packets [][]byte) []byte {
	units := make([][]byte, len(packets))
	size := 0

	for i, pkt := range packets {
		units[i] = opusAccessUnit(pkt)
		size += len(units[i])
	}

	out := make([]byte, 0, size)
	for _, unit := range units {
		out = append(out, unit...)
	}

	return out
}

// aacFrame builds a complete ADTS frame containing au as payload.
func aacFrame(t *testing.T, sampleRate int, au []byte) []byte {
	t.Helper()

	frame, err := mpeg4audio.ADTSPackets{{
		Type:          mpeg4audio.ObjectTypeAACLC,
		SampleRate:    sampleRate,
		ChannelConfig: 2,
		ChannelCount:  2, //nolint:staticcheck
		AU:            au,
	}}.Marshal()
	require.NoError(t, err)

	return frame
}

// opusAccessUnit builds an opus_access_unit (control header + packet).
func opusAccessUnit(packet []byte) []byte {
	au := substructs.OpusAccessUnit{
		ControlHeader: substructs.OpusControlHeader{
			PayloadSize: len(packet),
		},
		Packet: packet,
	}

	buf := make([]byte, au.MarshalSize())

	n, err := au.MarshalTo(buf)
	if err != nil {
		panic(err)
	}

	return buf[:n]
}

func addAACElementaryStream(t *testing.T, mux *astits.Muxer) {
	t.Helper()

	err := mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: audioTestPID,
		StreamType:    astits.StreamTypeAACAudio,
	})
	require.NoError(t, err)

	mux.SetPCRPID(audioTestPID)
}

func addOpusElementaryStream(t *testing.T, mux *astits.Muxer) {
	t.Helper()

	err := mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: audioTestPID,
		StreamType:    astits.StreamTypePrivateData,
		ElementaryStreamDescriptors: []*astits.Descriptor{
			{
				Length: 4,
				Tag:    astits.DescriptorTagRegistration,
				Registration: &astits.DescriptorRegistration{
					FormatIdentifier: 'O'<<24 | 'p'<<16 | 'u'<<8 | 's',
				},
			},
			{
				Length: 2,
				Tag:    astits.DescriptorTagExtension,
				Extension: &astits.DescriptorExtension{
					Tag:     0x80,
					Unknown: &[]uint8{2},
				},
			},
		},
	})
	require.NoError(t, err)

	mux.SetPCRPID(audioTestPID)
}

// writeAudioPES writes one PES packet carrying the given PTS and payload.
func writeAudioPES(t *testing.T, mux *astits.Muxer, pts int64, data []byte) {
	t.Helper()

	_, err := mux.WriteData(&astits.MuxerData{
		PID: audioTestPID,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: pts},
				},
				StreamID: 192,
			},
			Data: data,
		},
	})
	require.NoError(t, err)
}

func newAudioReader(t *testing.T, byts []byte) *mpegts.Reader {
	t.Helper()

	r, err := mpegts.NewReader(bytes.NewReader(byts))
	require.NoError(t, err)

	return r
}

// drainReader reads until EOF. Non-fatal decode errors are forwarded to onErr.
func drainReader(t *testing.T, r *mpegts.Reader, onErr func(error)) {
	t.Helper()

	r.OnDecodeError(onErr)

	for {
		err := r.Read()
		if errors.Is(err, io.EOF) {
			return
		}

		require.NoError(t, err)
	}
}

// nthPESPacketOffset returns the raw byte offset and the position, inside the
// 188-byte packet, where the codec payload of the nth (zero-based) PES packet of
// pid starts.
func nthPESPacketOffset(byts []byte, pid uint16, nth int) (packetOffset int, codecPayloadPos int) {
	seen := 0

	for i := 0; i+188 <= len(byts); i += 188 {
		pkt := byts[i : i+188]

		hasPayload := pkt[3]&0x10 > 0
		payloadStart := pkt[1]&0x40 > 0

		if !hasPayload || !payloadStart {
			continue
		}

		packetPID := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if packetPID != pid {
			continue
		}

		payloadPos := 4
		if pkt[3]&0x20 > 0 {
			payloadPos += 1 + int(pkt[4])
		}

		// PES start code (3 bytes) + stream id + PES packet length (2 bytes)
		// + optional header marker and length
		if payloadPos+9 > len(pkt) ||
			pkt[payloadPos] != 0x00 || pkt[payloadPos+1] != 0x00 || pkt[payloadPos+2] != 0x01 {
			continue
		}

		if seen == nth {
			optionalHeaderLen := int(pkt[payloadPos+8])
			return i, payloadPos + 9 + optionalHeaderLen
		}

		seen++
	}

	return -1, -1
}

// demuxedPTS returns the PTS list carried by the PES packets of the given PID.
func demuxedPTS(t *testing.T, byts []byte, pid uint16) []int64 {
	t.Helper()

	dem := astits.NewDemuxer(
		context.Background(),
		bytes.NewReader(byts),
		astits.DemuxerOptPacketSize(188))

	var out []int64

	for {
		data, err := dem.NextData()
		if err != nil {
			require.ErrorIs(t, err, astits.ErrNoMorePackets)
			return out
		}

		if data.PES != nil && data.PID == pid &&
			data.PES.Header.OptionalHeader != nil &&
			data.PES.Header.OptionalHeader.PTS != nil {
			out = append(out, data.PES.Header.OptionalHeader.PTS.Base)
		}
	}
}

// TestReaderMPEG4AudioFrameTiming constrains the relation between the PTS of a
// PES packet and the playback time of every AAC frame it contains. Frames of a
// same PES packet must not all reuse the packet start time: they advance by the
// number of encoded samples (1024 per access unit) on the 90kHz clock.
func TestReaderMPEG4AudioFrameTiming(t *testing.T) {
	t.Run("multiple frames in one PES", func(t *testing.T) {
		aus := [][]byte{
			bytes.Repeat([]byte{1}, 10),
			bytes.Repeat([]byte{2}, 12),
			bytes.Repeat([]byte{3}, 14),
		}

		frames := make([][]byte, len(aus))
		for i, au := range aus {
			frames[i] = aacFrame(t, 48000, au)
		}

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		// one PES containing three frames, stamped with the time of the first one
		writeAudioPES(t, mux, 90000, bytes.Join(frames, nil))
		// next PES, starting exactly three frame durations later
		writeAudioPES(t, mux, 90000+3*aacFrameDuration48k, frames[0])

		r := newAudioReader(t, buf.Bytes())

		type group struct {
			pts int64
			aus [][]byte
		}

		var recv []group

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, aus: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)
		require.Equal(t, []group{
			{90000, aus},
			{90000 + 3*aacFrameDuration48k, [][]byte{aus[0]}},
		}, recv)

		// reconstruct the per-frame playback timeline: every frame advances by
		// 1024 samples, so frames 1 and 2 must not inherit the PES start PTS
		var framePTS []int64

		for _, g := range recv {
			for i := range g.aus {
				framePTS = append(framePTS, g.pts+int64(i)*aacFrameDuration48k)
			}
		}

		require.Equal(t, []int64{
			90000,
			90000 + aacFrameDuration48k,
			90000 + 2*aacFrameDuration48k,
			90000 + 3*aacFrameDuration48k,
		}, framePTS)

		// explicitly guard against every frame reusing the packet start time
		require.Len(t, framePTS, 4)
		for i := 1; i < len(framePTS); i++ {
			require.Greater(t, framePTS[i], framePTS[i-1])
			require.NotEqual(t, framePTS[0], framePTS[i])
		}
	})

	t.Run("frame duration follows sample rate", func(t *testing.T) {
		// at 44.1kHz the 1024-sample duration does not divide the 90kHz clock
		// evenly: the reader truncates 92160000/44100 to 2089 clock units
		const sampleRate = 44100

		frameDuration := int64(mpeg4audio.SamplesPerAccessUnit * 90000 / sampleRate)
		require.Equal(t, int64(2089), frameDuration)

		f0 := aacFrame(t, sampleRate, bytes.Repeat([]byte{1}, 8))
		f1 := aacFrame(t, sampleRate, bytes.Repeat([]byte{2}, 8))
		f2 := aacFrame(t, sampleRate, bytes.Repeat([]byte{3}, 8))

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		writeAudioPES(t, mux, 90000, bytes.Join([][]byte{f0, f1}, nil))
		writeAudioPES(t, mux, 90000+2*frameDuration, f2)

		r := newAudioReader(t, buf.Bytes())

		var recvPTS []int64

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, _ [][]byte) error {
			recvPTS = append(recvPTS, pts)
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)
		require.Equal(t, []int64{90000, 90000 + 2*frameDuration}, recvPTS)
	})

	t.Run("writer reader round trip with multiple access units", func(t *testing.T) {
		track := &mpegts.Track{
			Codec: &codecs.MPEG4Audio{
				Config: mpeg4audio.AudioSpecificConfig{
					Type:          mpeg4audio.ObjectTypeAACLC,
					SampleRate:    48000,
					ChannelConfig: 2,
					ChannelCount:  2, //nolint:staticcheck
				},
			},
		}

		aus := [][]byte{
			bytes.Repeat([]byte{4}, 11),
			bytes.Repeat([]byte{5}, 13),
		}

		var buf bytes.Buffer
		w := mpegts.NewWriter(&buf, []*mpegts.Track{track})
		require.NotZero(t, track.PID)

		require.NoError(t, w.WriteMPEG4Audio(track, 90000, aus))
		require.NoError(t, w.WriteMPEG4Audio(track, 90000+2*aacFrameDuration48k, aus[:1]))

		// the writer must stamp the PES with the provided time
		require.Equal(t,
			[]int64{90000, 90000 + 2*aacFrameDuration48k},
			demuxedPTS(t, buf.Bytes(), track.PID))

		r := newAudioReader(t, buf.Bytes())

		var recv [][][]byte
		var recvPTS []int64

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recvPTS = append(recvPTS, pts)
			recv = append(recv, got)
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)
		require.Equal(t, []int64{90000, 90000 + 2*aacFrameDuration48k}, recvPTS)
		require.Equal(t, [][][]byte{aus, aus[:1]}, recv)
	})
}

// TestReaderMPEG4AudioTimestampWraparound verifies that playback order is
// preserved when PES timestamps cross the 33-bit boundary. Raw PTS wrap on the
// wire; the TimeDecoder extends them back into a strictly increasing timeline.
func TestReaderMPEG4AudioTimestampWraparound(t *testing.T) {
	frames := make([][]byte, 6)
	for i := range frames {
		frames[i] = aacFrame(t, 48000, bytes.Repeat([]byte{byte(i + 1)}, 8))
	}

	// start one frame duration before the 33-bit ceiling so the second PES wraps
	firstPTS := ts33Mask - aacFrameDuration48k + 1

	pess := []struct {
		pts    int64
		frames [][]byte
	}{
		{firstPTS, frames[0:2]},
		{(firstPTS + 2*aacFrameDuration48k) & ts33Mask, frames[2:4]},
		{(firstPTS + 4*aacFrameDuration48k) & ts33Mask, frames[4:6]},
	}

	var buf bytes.Buffer
	mux := astits.NewMuxer(context.Background(), &buf)
	addAACElementaryStream(t, mux)

	for _, pes := range pess {
		writeAudioPES(t, mux, pes.pts, bytes.Join(pes.frames, nil))
	}

	r := newAudioReader(t, buf.Bytes())

	type group struct {
		rawPTS int64
		count  int
	}

	var recv []group

	r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
		recv = append(recv, group{rawPTS: pts, count: len(got)})
		return nil
	})

	var decodeErrs []error
	drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

	require.Empty(t, decodeErrs)
	require.Len(t, recv, 3)

	// raw values actually wrap
	require.Equal(t, []int64{
		firstPTS,
		(firstPTS + 2*aacFrameDuration48k) & ts33Mask,
		(firstPTS + 4*aacFrameDuration48k) & ts33Mask,
	}, []int64{recv[0].rawPTS, recv[1].rawPTS, recv[2].rawPTS})
	require.Greater(t, recv[0].rawPTS, recv[1].rawPTS)

	// extending the timestamps restores the monotonic playback order
	dec := mpegts.NewTimeDecoder()

	decoded := make([]int64, 0, len(recv))

	for _, g := range recv {
		decoded = append(decoded, dec.Decode(g.rawPTS))
	}

	require.Equal(t, []int64{
		firstPTS,
		firstPTS + 2*aacFrameDuration48k,
		firstPTS + 4*aacFrameDuration48k,
	}, decoded)

	// per-frame timeline stays strictly increasing across the wrap
	var framePTS []int64

	for i, g := range recv {
		for j := 0; j < g.count; j++ {
			framePTS = append(framePTS, decoded[i]+int64(j)*aacFrameDuration48k)
		}
	}

	require.Len(t, framePTS, 6)

	for i := 1; i < len(framePTS); i++ {
		require.Equal(t, int64(aacFrameDuration48k), framePTS[i]-framePTS[i-1])
	}
}

// TestReaderMPEG4AudioIncompletePayload draws the boundary between already
// delivered whole frames, frames that can be recovered from later PES packets,
// and payloads reported as decode errors.
func TestReaderMPEG4AudioIncompletePayload(t *testing.T) {
	t.Run("partial frame at end of stream is retained without error", func(t *testing.T) {
		f0 := aacFrame(t, 48000, bytes.Repeat([]byte{1}, 20))
		f1 := aacFrame(t, 48000, bytes.Repeat([]byte{2}, 30))

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		// a whole frame followed by the beginning of a frame that never completes
		writeAudioPES(t, mux, 90000, bytes.Join([][]byte{f0, f1[:5]}, nil))

		r := newAudioReader(t, buf.Bytes())

		var callbacks int

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			callbacks++
			require.Equal(t, int64(90000), pts)
			require.Equal(t, [][]byte{bytes.Repeat([]byte{1}, 20)}, got)
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		// the whole frame is delivered, the truncated tail neither errors nor
		// produces a spurious frame
		require.Equal(t, 1, callbacks)
		require.Empty(t, decodeErrs)
	})

	t.Run("partial frame is completed by the next PES with its own time", func(t *testing.T) {
		aus := [][]byte{
			bytes.Repeat([]byte{1}, 20),
			bytes.Repeat([]byte{2}, 30),
			bytes.Repeat([]byte{3}, 10),
		}

		f0 := aacFrame(t, 48000, aus[0])
		f1 := aacFrame(t, 48000, aus[1])
		f2 := aacFrame(t, 48000, aus[2])

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		// f1 starts in the first PES and ends in the second; the second PES PTS
		// is the time of the first frame that starts inside it (f2)
		writeAudioPES(t, mux, 90000, bytes.Join([][]byte{f0, f1[:6]}, nil))
		writeAudioPES(t, mux, 90000+2*aacFrameDuration48k,
			bytes.Join([][]byte{f1[6:], f2}, nil))

		r := newAudioReader(t, buf.Bytes())

		type group struct {
			pts int64
			aus [][]byte
		}

		var recv []group

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, aus: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)

		// callbacks split at PES boundaries: the first PES only completes f0,
		// the second delivers the resumed f1 plus the newly started f2
		require.Equal(t, []group{
			{90000, [][]byte{aus[0]}},
			{90000 + aacFrameDuration48k, [][]byte{aus[1], aus[2]}},
		}, recv)

		// f1 kept the PTS of the PES where it started, f2 uses the new PES time
		require.Equal(t, int64(90000+aacFrameDuration48k), recv[1].pts)
		require.Equal(t, int64(90000+2*aacFrameDuration48k), recv[1].pts+aacFrameDuration48k)
	})

	t.Run("invalid payload errors and the next valid PES recovers on time", func(t *testing.T) {
		f0 := aacFrame(t, 48000, bytes.Repeat([]byte{0}, 16))
		f2 := aacFrame(t, 48000, bytes.Repeat([]byte{2}, 16))
		f3 := aacFrame(t, 48000, bytes.Repeat([]byte{3}, 16))

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		// a valid frame allows the reader to discover the codec configuration
		writeAudioPES(t, mux, 90000, f0)
		// then a byte that cannot start an ADTS frame
		writeAudioPES(t, mux, 90000+aacFrameDuration48k, []byte{0x00})
		// valid stream resumes; its PTS is independent of the bad packet
		writeAudioPES(t, mux, 90000+2*aacFrameDuration48k, f2)
		writeAudioPES(t, mux, 90000+3*aacFrameDuration48k, f3)

		r := newAudioReader(t, buf.Bytes())

		type group struct {
			pts int64
			aus [][]byte
		}

		var recv []group

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, aus: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		// exactly one error, attributed to the bad payload
		require.Len(t, decodeErrs, 1)
		require.EqualError(t, decodeErrs[0], "invalid ADTS: invalid length")

		// the bad PES delivered nothing; recovery keeps the correct timestamps
		require.Equal(t, []group{
			{90000, [][]byte{bytes.Repeat([]byte{0}, 16)}},
			{90000 + 2*aacFrameDuration48k, [][]byte{bytes.Repeat([]byte{2}, 16)}},
			{90000 + 3*aacFrameDuration48k, [][]byte{bytes.Repeat([]byte{3}, 16)}},
		}, recv)

		// the bad frame is lost but following frames keep their original time
		require.Equal(t, int64(2*aacFrameDuration48k), recv[1].pts-recv[0].pts)
		require.Equal(t, int64(aacFrameDuration48k), recv[2].pts-recv[1].pts)
	})
}

// TestReaderOpusFrameTiming constrains the relation between Opus PES timestamps
// and packet playback times. Each Opus packet represents 960 encoded samples, so
// packets packed into one PES must advance by 1800 90kHz units rather than all
// reusing the PES start time.
func TestReaderOpusFrameTiming(t *testing.T) {
	opusTrack := func() *mpegts.Track {
		return &mpegts.Track{
			Codec: &codecs.Opus{
				Desc: &substructs.OpusAudioDescriptor{
					ChannelConfigCode: 2,
				},
				ChannelCount: 2, //nolint:staticcheck
			},
		}
	}

	t.Run("multiple packets in one PES", func(t *testing.T) {
		packets := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10, 11, 12}}

		pesData := marshalOpusAccessUnits(packets)

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addOpusElementaryStream(t, mux)

		writeAudioPES(t, mux, 90000, pesData)
		writeAudioPES(t, mux, 90000+3*opusFrameDuration, opusAccessUnit(packets[0]))

		r := newAudioReader(t, buf.Bytes())

		type group struct {
			pts     int64
			packets [][]byte
		}

		var recv []group

		r.OnDataOpus(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, packets: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)
		require.Equal(t, []group{
			{90000, packets},
			{90000 + 3*opusFrameDuration, [][]byte{packets[0]}},
		}, recv)

		// per-packet playback timeline: 960 samples each, strictly increasing
		var packetPTS []int64

		for _, g := range recv {
			for i := range g.packets {
				packetPTS = append(packetPTS, g.pts+int64(i)*opusFrameDuration)
			}
		}

		require.Equal(t, []int64{
			90000,
			90000 + opusFrameDuration,
			90000 + 2*opusFrameDuration,
			90000 + 3*opusFrameDuration,
		}, packetPTS)

		for i := 1; i < len(packetPTS); i++ {
			require.Greater(t, packetPTS[i], packetPTS[i-1])
		}
	})

	t.Run("writer reader round trip with multiple packets", func(t *testing.T) {
		track := opusTrack()

		packets := [][]byte{{4, 3, 2, 1}, {8, 7, 6, 5}}

		var buf bytes.Buffer
		w := mpegts.NewWriter(&buf, []*mpegts.Track{track})
		require.NotZero(t, track.PID)

		require.NoError(t, w.WriteOpus(track, 90000, packets))
		require.NoError(t, w.WriteOpus(track, 90000+2*opusFrameDuration, packets[:1]))

		// the writer stamps each PES with the provided packet-start time
		require.Equal(t,
			[]int64{90000, 90000 + 2*opusFrameDuration},
			demuxedPTS(t, buf.Bytes(), track.PID))

		r := newAudioReader(t, buf.Bytes())

		var recvPTS []int64
		var recv [][][]byte

		r.OnDataOpus(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recvPTS = append(recvPTS, pts)
			recv = append(recv, got)
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Empty(t, decodeErrs)
		require.Equal(t, []int64{90000, 90000 + 2*opusFrameDuration}, recvPTS)
		require.Equal(t, [][][]byte{packets, packets[:1]}, recv)
	})
}

// TestReaderOpusTimestampWraparound verifies Opus playback order across the
// 33-bit PTS wrap, with two packets (each 960 samples) per PES.
func TestReaderOpusTimestampWraparound(t *testing.T) {
	packets := make([][]byte, 6)
	for i := range packets {
		packets[i] = []byte{byte(i + 1), 0x10, 0x20}
	}

	firstPTS := ts33Mask - opusFrameDuration + 1

	pess := []struct {
		pts     int64
		packets [][]byte
	}{
		{firstPTS, packets[0:2]},
		{(firstPTS + 2*opusFrameDuration) & ts33Mask, packets[2:4]},
		{(firstPTS + 4*opusFrameDuration) & ts33Mask, packets[4:6]},
	}

	var buf bytes.Buffer
	mux := astits.NewMuxer(context.Background(), &buf)
	addOpusElementaryStream(t, mux)

	for _, pes := range pess {
		writeAudioPES(t, mux, pes.pts, marshalOpusAccessUnits(pes.packets))
	}

	r := newAudioReader(t, buf.Bytes())

	type group struct {
		rawPTS int64
		count  int
	}

	var recv []group

	r.OnDataOpus(r.Tracks()[0], func(pts int64, got [][]byte) error {
		recv = append(recv, group{rawPTS: pts, count: len(got)})
		return nil
	})

	var decodeErrs []error
	drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

	require.Empty(t, decodeErrs)
	require.Len(t, recv, 3)

	// raw timestamps wrap on the wire and therefore appear to go backwards
	require.Greater(t, recv[0].rawPTS, recv[1].rawPTS)
	require.Equal(t, (firstPTS+2*opusFrameDuration)&ts33Mask, recv[1].rawPTS)

	// TimeDecoder restores a strictly increasing timeline
	dec := mpegts.NewTimeDecoder()

	decoded := make([]int64, 0, len(recv))

	for _, g := range recv {
		decoded = append(decoded, dec.Decode(g.rawPTS))
	}

	require.Equal(t, []int64{
		firstPTS,
		firstPTS + 2*opusFrameDuration,
		firstPTS + 4*opusFrameDuration,
	}, decoded)

	// every packet still represents 960 samples after the wrap
	var packetPTS []int64

	for i, g := range recv {
		for j := 0; j < g.count; j++ {
			packetPTS = append(packetPTS, decoded[i]+int64(j)*opusFrameDuration)
		}
	}

	require.Len(t, packetPTS, 6)

	for i := 1; i < len(packetPTS); i++ {
		require.Equal(t, int64(opusFrameDuration), packetPTS[i]-packetPTS[i-1])
	}
}

// TestReaderOpusIncompletePayload defines the boundary for truncated or invalid
// opus_access_unit payloads: a PES that fails to parse delivers nothing, while
// the following valid PES is still delivered at its own PTS.
func TestReaderOpusIncompletePayload(t *testing.T) {
	t.Run("truncated access unit errors without partial delivery", func(t *testing.T) {
		complete := opusAccessUnit([]byte{1, 2, 3, 4})

		// a control header announcing 5 payload bytes, with no payload attached
		truncated := []byte{0x7f, 0xe0, 0x05}

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addOpusElementaryStream(t, mux)

		// complete unit followed by a unit whose payload is missing
		writeAudioPES(t, mux, 90000, append(append([]byte{}, complete...), truncated...))

		r := newAudioReader(t, buf.Bytes())

		callbacks := 0
		r.OnDataOpus(r.Tracks()[0], func(_ int64, _ [][]byte) error {
			callbacks++
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		// nothing from the broken PES is delivered, including the complete unit
		require.Equal(t, 0, callbacks)
		require.Len(t, decodeErrs, 1)
		require.EqualError(t, decodeErrs[0], "buffer is too small")
	})

	t.Run("bad control header then recovery at correct time", func(t *testing.T) {
		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addOpusElementaryStream(t, mux)

		// 0x00 cannot start a control header (11-bit prefix 0x7ff)
		writeAudioPES(t, mux, 90000, []byte{0x00, 0x01, 0x02, 0x03})
		writeAudioPES(t, mux, 90000+opusFrameDuration, opusAccessUnit([]byte{5, 6, 7}))
		writeAudioPES(t, mux, 90000+2*opusFrameDuration, opusAccessUnit([]byte{8, 9, 10}))

		r := newAudioReader(t, buf.Bytes())

		type group struct {
			pts     int64
			packets [][]byte
		}

		var recv []group

		r.OnDataOpus(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, packets: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Len(t, decodeErrs, 1)
		require.EqualError(t, decodeErrs[0], "invalid control header: invalid prefix")

		// the bad PES is dropped; valid packets resume with correct timestamps
		require.Equal(t, []group{
			{90000 + opusFrameDuration, [][]byte{{5, 6, 7}}},
			{90000 + 2*opusFrameDuration, [][]byte{{8, 9, 10}}},
		}, recv)
	})
}

// TestReaderAudioRecoversAfterBadPacket corrupts one transport packet of an
// otherwise valid stream. The corrupted PES must be reported as a non-fatal
// decode error, and the audio that follows must still be delivered at its
// correct playback time, for both AAC and Opus.
func TestReaderAudioRecoversAfterBadPacket(t *testing.T) {
	t.Run("aac", func(t *testing.T) {
		f0 := aacFrame(t, 48000, bytes.Repeat([]byte{1}, 16))
		f1 := aacFrame(t, 48000, bytes.Repeat([]byte{2}, 16))
		f2 := aacFrame(t, 48000, bytes.Repeat([]byte{3}, 16))

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addAACElementaryStream(t, mux)

		writeAudioPES(t, mux, 90000, f0)
		writeAudioPES(t, mux, 90000+aacFrameDuration48k, f1)
		writeAudioPES(t, mux, 90000+2*aacFrameDuration48k, f2)

		byts := append([]byte(nil), buf.Bytes()...)

		// corrupt the second audio PES: the first one stays valid so that the
		// reader can discover the codec configuration during initialization
		off, payloadPos := nthPESPacketOffset(byts, audioTestPID, 1)
		require.NotEqual(t, -1, off)
		require.Equal(t, byte(0xFF), byts[off+payloadPos])
		byts[off+payloadPos] = 0x00

		r := newAudioReader(t, byts)

		type group struct {
			pts int64
			aus [][]byte
		}

		var recv []group

		r.OnDataMPEG4Audio(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, aus: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Len(t, decodeErrs, 1)
		require.EqualError(t, decodeErrs[0], "invalid ADTS: invalid syncword")

		// f0 is delivered before the corruption; f1 is lost; f2 stays on time
		require.Equal(t, []group{
			{90000, [][]byte{bytes.Repeat([]byte{1}, 16)}},
			{90000 + 2*aacFrameDuration48k, [][]byte{bytes.Repeat([]byte{3}, 16)}},
		}, recv)

		// recovery did not shift playback time
		require.Equal(t, int64(2*aacFrameDuration48k), recv[1].pts-recv[0].pts)
	})

	t.Run("opus", func(t *testing.T) {
		p0 := opusAccessUnit([]byte{1, 2, 3, 4})
		p1 := opusAccessUnit([]byte{5, 6, 7, 8})
		p2 := opusAccessUnit([]byte{9, 10, 11, 12})

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)
		addOpusElementaryStream(t, mux)

		writeAudioPES(t, mux, 90000, p0)
		writeAudioPES(t, mux, 90000+opusFrameDuration, p1)
		writeAudioPES(t, mux, 90000+2*opusFrameDuration, p2)

		byts := append([]byte(nil), buf.Bytes()...)

		// destroy the control header prefix of the first audio PES while keeping
		// the TS framing and PES header intact (Opus configuration comes from the
		// PMT descriptor, so no valid media PES is needed for initialization)
		off, payloadPos := nthPESPacketOffset(byts, audioTestPID, 0)
		require.NotEqual(t, -1, off)
		require.Equal(t, byte(0x7F), byts[off+payloadPos])
		byts[off+payloadPos] = 0x00

		r := newAudioReader(t, byts)

		type group struct {
			pts     int64
			packets [][]byte
		}

		var recv []group

		r.OnDataOpus(r.Tracks()[0], func(pts int64, got [][]byte) error {
			recv = append(recv, group{pts: pts, packets: got})
			return nil
		})

		var decodeErrs []error
		drainReader(t, r, func(err error) { decodeErrs = append(decodeErrs, err) })

		require.Len(t, decodeErrs, 1)
		require.EqualError(t, decodeErrs[0], "invalid control header: invalid prefix")

		// the corrupted PES is lost; subsequent packets keep their own PTS
		require.Equal(t, []group{
			{90000 + opusFrameDuration, [][]byte{{5, 6, 7, 8}}},
			{90000 + 2*opusFrameDuration, [][]byte{{9, 10, 11, 12}}},
		}, recv)
	})
}
