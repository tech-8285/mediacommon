package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	amp4 "github.com/abema/go-mp4"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
)

// tfhd and trun flags used by the fixtures of this file.
const (
	testTfhdDefaultDuration = 0x000008
	testTfhdDefaultSize     = 0x000010
	testTfhdDefaultFlags    = 0x000020

	testTrunDataOffset      = 0x000001
	testTrunSampleDuration  = 0x000100
	testTrunSampleSize      = 0x000200
	testTrunSampleFlags     = 0x000400
	testTrunSamplePTSOffset = 0x000800

	testSampleNonSync = 1 << 16
)

func testUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func testInt32(v int32) []byte {
	return testUint32(uint32(v))
}

func testUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// testBox builds a plain box.
func testBox(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, part := range payload {
		body = append(body, part...)
	}

	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

// testFullBox builds a full box.
func testFullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	header := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return testBox(typ, append([][]byte{header}, payload...)...)
}

func testTfhd(trackID uint32, flags uint32, fields ...[]byte) []byte {
	return testFullBox("tfhd", 0, flags, append([][]byte{testUint32(trackID)}, fields...)...)
}

func testTfdt(baseTime uint64) []byte {
	return testFullBox("tfdt", 1, 0, testUint64(baseTime))
}

func testTrun(version byte, flags uint32, sampleCount uint32, fields ...[]byte) []byte {
	return testFullBox("trun", version, flags, append([][]byte{testUint32(sampleCount)}, fields...)...)
}

func testMoof(sequenceNumber uint32, trafs ...[]byte) []byte {
	children := append([][]byte{testFullBox("mfhd", 0, 0, testUint32(sequenceNumber))}, trafs...)
	return testBox("moof", children...)
}

// testTrack is a track described by a fixture.
type testTrack struct {
	id       int
	baseTime uint64

	// defaultMask states which default sample attributes are carried by tfhd
	// instead of by every trun entry.
	defaultMask     uint32
	defaultDuration uint32
	defaultSize     uint32
	defaultFlags    uint32

	samples []testSample
}

// testSample is a sample described by a fixture.
type testSample struct {
	// duration and isNonSync are only used when the corresponding attribute
	// is not carried by tfhd.
	duration  uint32
	ptsOffset int32
	isNonSync bool
	payload   []byte
}

func (t *testTrack) durationInTrun() bool {
	return t.defaultMask&testTfhdDefaultDuration == 0
}

func (t *testTrack) sizeInTrun() bool {
	return t.defaultMask&testTfhdDefaultSize == 0
}

func (t *testTrack) flagsInTrun() bool {
	return t.defaultMask&testTfhdDefaultFlags == 0
}

// testChunk places the payload of a sample in the mdat.
type testChunk struct {
	trackIdx  int
	sampleIdx int
}

// testPlacement is the position occupied by a sample payload inside a fragment.
type testPlacement struct {
	trackID   int
	sampleIdx int
	offset    int
}

// testFragment builds a fMP4 fragment without the mdat box and lets trafs
// compute data offsets relative to the start of the moof, which is placed at
// offset 0.
func testFragment(t *testing.T, trafs func(mdatStart int32) [][]byte) []byte {
	t.Helper()

	// the moof size does not depend on the value of the data offsets, so the
	// second pass can use the size of the first one.
	probe := testMoof(1, trafs(0)...)
	start := int32(len(probe) + 8)
	moof := testMoof(1, trafs(start)...)
	require.Equal(t, len(probe), len(moof))

	return moof
}

// testBuildFragment builds a fragment whose mdat payload follows chunks in
// order, while trun entries follow the sample order of every track.
func testBuildFragment(t *testing.T, tracks []*testTrack, chunks []testChunk) ([]byte, []testPlacement) {
	t.Helper()

	return testBuildFragmentWithPrefix(t, tracks, chunks, nil)
}

// testBuildFragmentWithPrefix is testBuildFragment with a box placed before
// the moof. data offsets are relative to the start of the moof, so the prefix
// must not change them, only the absolute position of the mdat.
func testBuildFragmentWithPrefix(
	t *testing.T,
	tracks []*testTrack,
	chunks []testChunk,
	prefix []byte,
) ([]byte, []testPlacement) {
	t.Helper()

	positions := make(map[[2]int]int, len(chunks))
	var mdat []byte

	for _, c := range chunks {
		key := [2]int{c.trackIdx, c.sampleIdx}
		require.NotContains(t, positions, key)

		sample := tracks[c.trackIdx].samples[c.sampleIdx]
		positions[key] = len(mdat)
		mdat = append(mdat, sample.payload...)
	}

	// every sample must be stored somewhere.
	for ti, track := range tracks {
		for si := range track.samples {
			require.Contains(t, positions, [2]int{ti, si})
		}
	}

	buildTrafs := func(mdatStart int32) [][]byte {
		trafs := make([][]byte, len(tracks))

		for ti, track := range tracks {
			// consecutive samples that are also consecutive in the mdat share
			// a trun; otherwise a new trun with its own data offset is needed.
			var runs [][]int
			var cur []int

			for si := range track.samples {
				if len(cur) != 0 {
					prev := cur[len(cur)-1]
					prevEnd := positions[[2]int{ti, prev}] + len(track.samples[prev].payload)
					if prevEnd != positions[[2]int{ti, si}] {
						runs = append(runs, cur)
						cur = nil
					}
				}
				cur = append(cur, si)
			}
			runs = append(runs, cur)

			children := make([][]byte, 0, 2+len(runs))
			children = append(children, testTfhd(
				uint32(track.id),
				track.defaultMask,
				testTfhdFields(track)...,
			))
			children = append(children, testTfdt(track.baseTime))

			for _, run := range runs {
				children = append(children, testTrun(
					1,
					uint32(testTrunDataOffset|testTrunSamplePTSOffset)|
						testTrunAttributeFlags(track),
					uint32(len(run)),
					testTrunFields(track, run, mdatStart+int32(positions[[2]int{ti, run[0]}]))...,
				))
			}

			trafs[ti] = testBox("traf", children...)
		}

		return trafs
	}

	moof := testFragment(t, buildTrafs)
	enc := append(append(append([]byte{}, prefix...), moof...), testBox("mdat", mdat)...)
	mdatStart := len(prefix) + len(moof) + 8

	placements := make([]testPlacement, 0, len(chunks))
	for _, c := range chunks {
		placements = append(placements, testPlacement{
			trackID:   tracks[c.trackIdx].id,
			sampleIdx: c.sampleIdx,
			offset:    mdatStart + positions[[2]int{c.trackIdx, c.sampleIdx}],
		})
	}

	return enc, placements
}

func testTfhdFields(track *testTrack) [][]byte {
	var fields [][]byte
	if track.defaultMask&testTfhdDefaultDuration != 0 {
		fields = append(fields, testUint32(track.defaultDuration))
	}
	if track.defaultMask&testTfhdDefaultSize != 0 {
		fields = append(fields, testUint32(track.defaultSize))
	}
	if track.defaultMask&testTfhdDefaultFlags != 0 {
		fields = append(fields, testUint32(track.defaultFlags))
	}
	return fields
}

func testTrunAttributeFlags(track *testTrack) uint32 {
	var flags uint32
	if track.durationInTrun() {
		flags |= testTrunSampleDuration
	}
	if track.sizeInTrun() {
		flags |= testTrunSampleSize
	}
	if track.flagsInTrun() {
		flags |= testTrunSampleFlags
	}
	return flags
}

func testTrunFields(track *testTrack, run []int, firstOffset int32) [][]byte {
	fields := [][]byte{testInt32(firstOffset)}

	for _, si := range run {
		sample := track.samples[si]

		if track.durationInTrun() {
			fields = append(fields, testUint32(sample.duration))
		}
		if track.sizeInTrun() {
			fields = append(fields, testUint32(uint32(len(sample.payload))))
		}
		if track.flagsInTrun() {
			var sampleFlags uint32
			if sample.isNonSync {
				sampleFlags = testSampleNonSync
			}
			fields = append(fields, testUint32(sampleFlags))
		}
		fields = append(fields, testInt32(sample.ptsOffset))
	}

	return fields
}

// testExpectedParts converts the fixture description into the fMP4 parts that
// a correct decoder must return.
func testExpectedParts(tracks []*testTrack) fmp4.Parts {
	part := &fmp4.Part{SequenceNumber: 1}

	for _, track := range tracks {
		pt := &fmp4.PartTrack{
			ID:       track.id,
			BaseTime: track.baseTime,
		}

		for _, sample := range track.samples {
			duration := sample.duration
			if !track.durationInTrun() {
				duration = track.defaultDuration
			}

			// sample flags declared by tfhd apply to every sample, regardless
			// of what the fixture says about a single sample.
			isNonSync := sample.isNonSync
			if !track.flagsInTrun() {
				isNonSync = track.defaultFlags&testSampleNonSync != 0
			}

			pt.Samples = append(pt.Samples, &fmp4.Sample{
				Duration:        duration,
				PTSOffset:       sample.ptsOffset,
				IsNonSyncSample: isNonSync,
				Payload:         sample.payload,
			})
		}

		part.Tracks = append(part.Tracks, pt)
	}

	return fmp4.Parts{part}
}

// testTimes returns the decode time and the presentation time of every sample
// of a track.
func testTimes(track *fmp4.PartTrack) (decode []uint64, presentation []int64) {
	decodeTime := track.BaseTime

	for _, sample := range track.Samples {
		decode = append(decode, decodeTime)
		presentation = append(presentation, int64(decodeTime)+int64(sample.PTSOffset))
		decodeTime += uint64(sample.Duration)
	}

	return
}

func testTrackByID(tracks []*testTrack, id int) *testTrack {
	for _, track := range tracks {
		if track.id == id {
			return track
		}
	}
	panic("track not found")
}

func testSampleByIndex(track *fmp4.PartTrack, idx int) *fmp4.Sample {
	return track.Samples[idx]
}

// testRequireContainersConsistent verifies that every payload is stored at the
// position claimed by the fixture, and that every decoded sample carries the
// payload of its own track.
func testRequireContainersConsistent(
	t *testing.T,
	enc []byte,
	tracks []*testTrack,
	placements []testPlacement,
	parts fmp4.Parts,
) {
	t.Helper()

	require.Len(t, parts, 1)
	require.Len(t, parts[0].Tracks, len(tracks))

	decoded := make(map[int]*fmp4.PartTrack, len(parts[0].Tracks))
	for _, track := range parts[0].Tracks {
		require.NotContains(t, decoded, track.ID)
		decoded[track.ID] = track
	}

	// the payloads of the fixtures must be distinguishable, otherwise a
	// cross-track swap could not be detected.
	owners := make(map[string][2]int)
	for _, track := range tracks {
		for si, sample := range track.samples {
			key := string(sample.payload)
			require.NotContains(t, owners, key, "payload of track %d sample %d is reused", track.id, si)
			owners[key] = [2]int{track.id, si}
		}
	}

	for _, placement := range placements {
		payload := testTrackByID(tracks, placement.trackID).samples[placement.sampleIdx].payload

		// the container position holds exactly this sample payload.
		require.LessOrEqual(t, placement.offset+len(payload), len(enc))
		require.Equal(t, payload, enc[placement.offset:placement.offset+len(payload)])

		// the decoded sample at the same index of the same track carries it.
		require.Less(t, placement.sampleIdx, len(decoded[placement.trackID].Samples))
		sample := testSampleByIndex(decoded[placement.trackID], placement.sampleIdx)
		require.Equal(t, payload, sample.Payload)

		// and it belongs to that sample only: no other track may own it.
		require.Equal(t, placement.trackID, owners[string(sample.Payload)][0])

		// every decoded sample must be reachable from the container.
		require.True(t, bytes.Contains(enc, sample.Payload))
	}
}

func TestPartsDataOffsetIsRelativeToMoof(t *testing.T) {
	tracks := []*testTrack{
		{
			id:              1,
			baseTime:        90000,
			defaultMask:     testTfhdDefaultDuration | testTfhdDefaultFlags,
			defaultDuration: 30,
			defaultFlags:    testSampleNonSync,
			samples: []testSample{
				{ptsOffset: 0, payload: []byte{0x11, 0x12}},
				{ptsOffset: -5, payload: []byte{0x13, 0x14, 0x15}},
			},
		},
		{
			id:       2,
			baseTime: 0,
			samples: []testSample{
				{duration: 10, ptsOffset: 0, payload: []byte{0x21}},
				{duration: 10, ptsOffset: 3, payload: []byte{0x22, 0x23}},
			},
		},
	}

	chunks := []testChunk{{0, 0}, {1, 0}, {0, 1}, {1, 1}}

	// a box of a size that would shift every offset if data offsets were
	// interpreted relative to the start of the buffer instead of the moof.
	prefix := testBox("free", make([]byte, 24))

	enc, placements := testBuildFragmentWithPrefix(t, tracks, chunks, prefix)

	var parts fmp4.Parts
	err := parts.Unmarshal(enc)
	require.NoError(t, err)
	require.Equal(t, testExpectedParts(tracks), parts)

	testRequireContainersConsistent(t, enc, tracks, placements, parts)

	// the data offsets stored in the fragment must be relative to the moof,
	// not to the start of the buffer.
	moofStart := len(prefix)

	var truns []*amp4.Trun
	_, err = amp4.ReadBoxStructure(bytes.NewReader(enc), func(h *amp4.ReadHandle) (any, error) {
		if h.BoxInfo.Type == amp4.BoxTypeMoof() {
			require.Equal(t, uint64(moofStart), h.BoxInfo.Offset)
		}
		if h.BoxInfo.Type == amp4.BoxTypeTrun() {
			box, _, err2 := h.ReadPayload()
			require.NoError(t, err2)
			truns = append(truns, box.(*amp4.Trun))
			return nil, nil
		}
		return h.Expand()
	})
	require.NoError(t, err)
	require.Len(t, truns, len(placements))

	// every stored data offset, resolved against the moof start, must land
	// exactly on the payload of a distinct sample.
	hit := make(map[int]bool, len(placements))

	for _, trun := range truns {
		abs := moofStart + int(trun.DataOffset)
		matched := false
		for _, placement := range placements {
			if placement.offset == abs {
				require.False(t, hit[abs], "offset %d used twice", abs)
				hit[abs] = true
				matched = true
			}
		}
		require.True(t, matched, "data offset %d does not land on a sample", abs)
	}

	require.Len(t, hit, len(placements))
}

func testPayload(track, sample, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(0x10*(track+1) + sample)
	}
	return out
}

func TestPartsMultiTrackInterleavedRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		tracks []*testTrack
		chunks []testChunk
	}{
		{
			"interleaved samples, per track default attributes",
			[]*testTrack{
				{
					id:              1,
					baseTime:        90000,
					defaultMask:     testTfhdDefaultDuration | testTfhdDefaultSize | testTfhdDefaultFlags,
					defaultDuration: 30,
					defaultSize:     3,
					// tfhd flags apply to every sample of track 1.
					defaultFlags: testSampleNonSync,
					samples: []testSample{
						{ptsOffset: 0, payload: testPayload(0, 0, 3)},
						{ptsOffset: -15, payload: testPayload(0, 1, 3)},
						{ptsOffset: 15, payload: testPayload(0, 2, 3)},
					},
				},
				{
					id:       2,
					baseTime: 5000,
					samples: []testSample{
						{duration: 20, ptsOffset: 5, payload: testPayload(1, 0, 2)},
						{duration: 20, ptsOffset: -7, isNonSync: true, payload: testPayload(1, 1, 4)},
						{duration: 20, ptsOffset: 0, payload: testPayload(1, 2, 3)},
					},
				},
			},
			// A0, B0, then two consecutive samples of A, then two of B.
			[]testChunk{
				{0, 0},
				{1, 0},
				{0, 1},
				{0, 2},
				{1, 1},
				{1, 2},
			},
		},
		{
			"data offsets not following the sample order",
			[]*testTrack{
				{
					id:              1,
					baseTime:        90000,
					defaultMask:     testTfhdDefaultDuration | testTfhdDefaultSize | testTfhdDefaultFlags,
					defaultDuration: 30,
					defaultSize:     3,
					defaultFlags:    0,
					samples: []testSample{
						{ptsOffset: -15, payload: testPayload(0, 0, 3)},
						{ptsOffset: 15, payload: testPayload(0, 1, 3)},
					},
				},
				{
					id:       2,
					baseTime: 44100,
					samples: []testSample{
						{duration: 20, ptsOffset: 5, payload: testPayload(1, 0, 2)},
						{duration: 20, ptsOffset: -7, isNonSync: true, payload: testPayload(1, 1, 2)},
					},
				},
			},
			// the first sample of track 1 is stored after its second sample.
			[]testChunk{
				{0, 1},
				{1, 0},
				{0, 0},
				{1, 1},
			},
		},
		{
			"mixed default and explicit attributes",
			[]*testTrack{
				{
					id:          1,
					baseTime:    180000,
					defaultMask: testTfhdDefaultSize,
					defaultSize: 4,
					samples: []testSample{
						{duration: 40, ptsOffset: 0, payload: testPayload(0, 0, 4)},
						{duration: 40, ptsOffset: -40, isNonSync: true, payload: testPayload(0, 1, 4)},
					},
				},
				{
					id:              2,
					baseTime:        0,
					defaultMask:     testTfhdDefaultDuration | testTfhdDefaultFlags,
					defaultDuration: 512,
					defaultFlags:    testSampleNonSync,
					samples: []testSample{
						{ptsOffset: 3, payload: testPayload(1, 0, 1)},
						{ptsOffset: 0, payload: testPayload(1, 1, 5)},
					},
				},
				{
					id:       3,
					baseTime: 0,
					samples: []testSample{
						{duration: 10, ptsOffset: -2, payload: testPayload(2, 0, 2)},
						{duration: 10, ptsOffset: 0, payload: testPayload(2, 1, 3)},
						{duration: 10, ptsOffset: 2, payload: testPayload(2, 2, 4)},
					},
				},
			},
			[]testChunk{
				{0, 0},
				{1, 0},
				{2, 0},
				{0, 1},
				{2, 1},
				{1, 1},
				{2, 2},
			},
		},
	}

	for _, ca := range cases {
		t.Run(ca.name, func(t *testing.T) {
			enc, placements := testBuildFragment(t, ca.tracks, ca.chunks)
			expected := testExpectedParts(ca.tracks)

			var parts fmp4.Parts
			err := parts.Unmarshal(enc)
			require.NoError(t, err)
			require.Equal(t, expected, parts)

			testRequireContainersConsistent(t, enc, ca.tracks, placements, parts)

			// decode times and presentation times are derived from the track
			// fields and must be unique per track.
			for _, track := range parts[0].Tracks {
				decode, presentation := testTimes(track)
				require.Len(t, decode, len(track.Samples))
				for i := 1; i < len(decode); i++ {
					require.Greater(t, decode[i], decode[i-1])
				}
				require.Len(t, presentation, len(track.Samples))
			}

			// a second round trip must neither move samples to another track
			// nor change their payload or timing.
			var buf seekablebuffer.Buffer
			err = parts.Marshal(&buf)
			require.NoError(t, err)

			var parts2 fmp4.Parts
			err = parts2.Unmarshal(buf.Bytes())
			require.NoError(t, err)
			require.Equal(t, parts, parts2)

			for ti, track := range parts[0].Tracks {
				decode, presentation := testTimes(track)
				decode2, presentation2 := testTimes(parts2[0].Tracks[ti])
				require.Equal(t, decode, decode2)
				require.Equal(t, presentation, presentation2)
			}

			// every payload is stored exactly once in the media data of the
			// re-encoded container, whatever track it belongs to.
			var mdat []byte
			_, err = amp4.ReadBoxStructure(bytes.NewReader(buf.Bytes()), func(h *amp4.ReadHandle) (any, error) {
				if h.BoxInfo.Type == amp4.BoxTypeMdat() {
					var out bytes.Buffer
					_, err2 := h.ReadData(&out)
					require.NoError(t, err2)
					mdat = out.Bytes()
					return nil, nil
				}
				return h.Expand()
			})
			require.NoError(t, err)
			require.NotEmpty(t, mdat)

			for _, track := range ca.tracks {
				for _, sample := range track.samples {
					require.Equal(t, 1, bytes.Count(mdat, sample.payload),
						"payload of track %d stored %d times", track.id, bytes.Count(mdat, sample.payload))
				}
			}
		})
	}
}

func TestPartsNegativeCompositionOffset(t *testing.T) {
	tracks := []*testTrack{{
		id:       7,
		baseTime: 90000,
		samples: []testSample{
			{duration: 30, ptsOffset: 0, payload: []byte{0x01, 0x02, 0x03, 0x04}},
			{duration: 30, ptsOffset: -45, isNonSync: true, payload: []byte{0x05, 0x06, 0x07}},
			{duration: 30, ptsOffset: 15, payload: []byte{0x08, 0x09, 0x0A, 0x0B, 0x0C}},
		},
	}}

	enc, placements := testBuildFragment(t, tracks, []testChunk{{0, 0}, {0, 1}, {0, 2}})

	var parts fmp4.Parts
	err := parts.Unmarshal(enc)
	require.NoError(t, err)
	require.Equal(t, testExpectedParts(tracks), parts)

	track := parts[0].Tracks[0]

	// decode times keep flowing from the non-zero base decode time.
	decode, presentation := testTimes(track)
	require.Equal(t, []uint64{90000, 90030, 90060}, decode)

	// negative composition offsets only move presentation times backwards,
	// without touching decode times.
	require.Equal(t, []int64{90000, 89985, 90075}, presentation)

	// the negative offset must not change the bytes the sample points to.
	testRequireContainersConsistent(t, enc, tracks, placements, parts)
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, track.Samples[0].Payload)
	require.Equal(t, []byte{0x05, 0x06, 0x07}, track.Samples[1].Payload)
	require.Equal(t, []byte{0x08, 0x09, 0x0A, 0x0B, 0x0C}, track.Samples[2].Payload)
	require.Equal(t, uint64(90000), track.BaseTime)

	// negative offsets must be re-encoded as signed version 1 entries.
	var buf seekablebuffer.Buffer
	err = parts.Marshal(&buf)
	require.NoError(t, err)

	var truns []*amp4.Trun
	_, err = amp4.ReadBoxStructure(bytes.NewReader(buf.Bytes()), func(h *amp4.ReadHandle) (any, error) {
		if h.BoxInfo.Type == amp4.BoxTypeTrun() {
			box, _, err2 := h.ReadPayload()
			require.NoError(t, err2)
			truns = append(truns, box.(*amp4.Trun))
			return nil, nil
		}
		return h.Expand()
	})
	require.NoError(t, err)

	require.Len(t, truns, 1)
	require.Equal(t, uint8(1), truns[0].Version)
	require.NotZero(t, (uint16(truns[0].Flags[1])<<8|uint16(truns[0].Flags[2]))&testTrunSamplePTSOffset)
	require.Len(t, truns[0].Entries, 3)
	require.Equal(t, int32(0), truns[0].Entries[0].SampleCompositionTimeOffsetV1)
	require.Equal(t, int32(-45), truns[0].Entries[1].SampleCompositionTimeOffsetV1)
	require.Equal(t, int32(15), truns[0].Entries[2].SampleCompositionTimeOffsetV1)
	require.Equal(t, uint32(4), truns[0].Entries[0].SampleSize)
	require.Equal(t, uint32(3), truns[0].Entries[1].SampleSize)
	require.Equal(t, uint32(5), truns[0].Entries[2].SampleSize)

	var parts2 fmp4.Parts
	err = parts2.Unmarshal(buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, parts, parts2)

	decode2, presentation2 := testTimes(parts2[0].Tracks[0])
	require.Equal(t, decode, decode2)
	require.Equal(t, presentation, presentation2)
}

func TestPartsUnmarshalFailureBoundaries(t *testing.T) {
	// a single sample of two bytes, all attributes explicit.
	explicitTrun := func(offset int32, count uint32, size uint32) []byte {
		return testTrun(0, testTrunDataOffset|testTrunSampleDuration|testTrunSampleSize,
			count, testInt32(offset), testUint32(30), testUint32(size))
	}

	cases := []struct {
		name        string
		build       func(t *testing.T) []byte
		errContains string
	}{
		{
			"traf without tfdt",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), explicitTrun(start, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"parse error",
		},
		{
			"traf without tfhd",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfdt(0), explicitTrun(start, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"unexpected trun",
		},
		{
			"trun without data offset flag",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(_ int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						testTrun(0, testTrunSampleDuration|testTrunSampleSize, 1,
							testUint32(30), testUint32(2)))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"unsupported flags",
		},
		{
			"trun truncated before the sample count",
			func(t *testing.T) []byte {
				t.Helper()
				mfhd := testFullBox("mfhd", 0, 0, testUint32(1))
				// the trun box ends right after its version and flags.
				trun := testFullBox("trun", 0, testTrunDataOffset)
				bad := testBox("moof", mfhd, testBox("traf", testTfhd(1, 0), testTfdt(0), trun))
				return append(bad, testBox("mdat", []byte{1, 2})...)
			},
			"",
		},
		{
			"trun declaring more samples than the fragment holds",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					// a single entry, but two samples declared and a default
					// size of two bytes: the second sample has no bytes left.
					trun := testTrun(0, testTrunDataOffset|testTrunSampleDuration, 2,
						testInt32(start), testUint32(30), testUint32(30))
					return [][]byte{testBox("traf",
						testTfhd(1, testTfhdDefaultSize, testUint32(2)),
						testTfdt(0), trun)}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid sample size",
		},
		{
			"data offset beyond the fragment",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start+1000, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid data_offset / moof_offset",
		},
		{
			"negative data offset",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(_ int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(-8, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid data_offset / moof_offset",
		},
		{
			"sample size zero",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 0))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid sample size",
		},
		{
			"sample size larger than the mdat",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 8))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid sample size",
		},
		{
			"default sample size zero",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					trun := testTrun(0, testTrunDataOffset|testTrunSampleDuration, 1,
						testInt32(start), testUint32(30))
					return [][]byte{testBox("traf", testTfhd(1, testTfhdDefaultSize, testUint32(0)),
						testTfdt(0), trun)}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"invalid sample size",
		},
		{
			"sample count above the limit",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(_ int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						testTrun(1, testTrunDataOffset, 19201, testInt32(0)))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"sample count (19201) exceeds maximum (19200)",
		},
		{
			"duplicated tfhd",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"unexpected tfhd",
		},
		{
			"duplicated tfdt",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
				return append(moof, testBox("mdat", []byte{1, 2})...)
			},
			"unexpected tfdt",
		},
		{
			"traf without mfhd",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
				// remove the mfhd box, which occupies the bytes right after the
				// moof header.
				bad := testBox("moof", moof[8+16:])
				return append(bad, testBox("mdat", []byte{1, 2})...)
			},
			"unexpected traf",
		},
		{
			"moof without mdat",
			func(t *testing.T) []byte {
				t.Helper()
				return testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
			},
			"invalid data_offset / moof_offset",
		},
		{
			"moof without mdat pointing inside itself",
			func(t *testing.T) []byte {
				t.Helper()
				return testFragment(t, func(_ int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(0, 1, 2))}
				})
			},
			"decode error",
		},
		{
			"two moofs without mdat",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
				return append(moof, moof...)
			},
			"unexpected moof",
		},
		{
			"mdat before moof",
			func(t *testing.T) []byte {
				t.Helper()
				return testBox("mdat", []byte{1, 2})
			},
			"unexpected mdat",
		},
		{
			"moof size beyond the fragment",
			func(t *testing.T) []byte {
				t.Helper()
				moof := testFragment(t, func(start int32) [][]byte {
					return [][]byte{testBox("traf", testTfhd(1, 0), testTfdt(0),
						explicitTrun(start, 1, 2))}
				})
				// cut inside the moof, so that the declared box size exceeds
				// the amount of available bytes.
				return moof[:len(moof)-10]
			},
			"box size exceeds file size",
		},
	}

	for _, ca := range cases {
		t.Run(ca.name, func(t *testing.T) {
			var parts fmp4.Parts
			err := parts.Unmarshal(ca.build(t))
			require.Error(t, err)
			if ca.errContains != "" {
				require.Contains(t, err.Error(), ca.errContains)
			}
		})
	}
}

// TestPartsUnmarshalTruncated checks that cutting a valid fragment at any
// position either fails or only yields samples fully contained in the prefix.
func TestPartsUnmarshalTruncated(t *testing.T) {
	tracks := []*testTrack{
		{
			id:              1,
			baseTime:        90000,
			defaultMask:     testTfhdDefaultDuration | testTfhdDefaultSize | testTfhdDefaultFlags,
			defaultDuration: 30,
			defaultSize:     2,
			defaultFlags:    testSampleNonSync,
			samples: []testSample{
				{ptsOffset: 0, payload: []byte{0xA0, 0xA1}},
				{ptsOffset: -10, payload: []byte{0xA2, 0xA3}},
			},
		},
		{
			id:       2,
			baseTime: 1000,
			samples: []testSample{
				{duration: 20, ptsOffset: 5, payload: []byte{0xB0, 0xB1, 0xB2}},
				{duration: 20, ptsOffset: 0, payload: []byte{0xB3}},
			},
		},
	}

	enc, _ := testBuildFragment(t, tracks, []testChunk{
		{0, 0}, {1, 0}, {0, 1}, {1, 1},
	})

	// a prefix shorter than a box header cannot contain anything, so it
	// decodes to zero parts; any longer incomplete prefix must be rejected
	// instead of producing samples that only partially exist.
	for n := range len(enc) {
		var parts fmp4.Parts
		err := parts.Unmarshal(enc[:n])

		if n < 8 {
			require.NoError(t, err)
			require.Empty(t, parts)
			continue
		}

		require.Error(t, err, "prefix of %d bytes out of %d must not decode", n, len(enc))
	}

	var parts fmp4.Parts
	err := parts.Unmarshal(enc)
	require.NoError(t, err)
	require.Equal(t, testExpectedParts(tracks), parts)

	for _, part := range parts {
		for _, track := range part.Tracks {
			for _, sample := range track.Samples {
				require.NotEmpty(t, sample.Payload)
				require.True(t, bytes.Contains(enc, sample.Payload))
			}
		}
	}
}

// Handwritten fragments are kept as byte literals so that the expected
// interpretation of every field is frozen independently from the encoders
// under test.

// testEncodedInterleavedFragment stores:
//
//	mdat: A0|B0|A1|B1   (9 bytes)
//
// track 1 carries default duration, size and flags in tfhd and expresses its
// samples with two data-only truns; the first sample is stored after the
// second one, so a decoder must follow data offsets, not mdat order.
// track 2 carries explicit entries and offsets whose composition time may be
// negative.
var testEncodedInterleavedFragment = []byte{
	0x00, 0x00, 0x00, 0xf4, 0x6d, 0x6f, 0x6f, 0x66, 0x00, 0x00, 0x00, 0x10, 0x6d, 0x66, 0x68, 0x64,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x68, 0x74, 0x72, 0x61, 0x66,
	0x00, 0x00, 0x00, 0x1c, 0x74, 0x66, 0x68, 0x64, 0x00, 0x00, 0x00, 0x38, 0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x1e, 0x00, 0x00, 0x00, 0x03, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x14,
	0x74, 0x66, 0x64, 0x74, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x5f, 0x90,
	0x00, 0x00, 0x00, 0x18, 0x74, 0x72, 0x75, 0x6e, 0x01, 0x00, 0x08, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x01, 0x01, 0xff, 0xff, 0xff, 0xf1, 0x00, 0x00, 0x00, 0x18, 0x74, 0x72, 0x75, 0x6e,
	0x01, 0x00, 0x08, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xfc, 0x00, 0x00, 0x00, 0x0f,
	0x00, 0x00, 0x00, 0x74, 0x74, 0x72, 0x61, 0x66, 0x00, 0x00, 0x00, 0x10, 0x74, 0x66, 0x68, 0x64,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x14, 0x74, 0x66, 0x64, 0x74,
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x13, 0x88, 0x00, 0x00, 0x00, 0x24,
	0x74, 0x72, 0x75, 0x6e, 0x01, 0x00, 0x0f, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xff,
	0x00, 0x00, 0x00, 0x14, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05,
	0x00, 0x00, 0x00, 0x24, 0x74, 0x72, 0x75, 0x6e, 0x01, 0x00, 0x0f, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x01, 0x04, 0x00, 0x00, 0x00, 0x14, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, 0x00, 0x00,
	0xff, 0xff, 0xff, 0xf9, 0x00, 0x00, 0x00, 0x12, 0x6d, 0x64, 0x61, 0x74, 0xa0, 0xa1, 0xa2, 0xb0,
	0xb1, 0xa3, 0xa4, 0xa5, 0xb2, 0xb3,
}

var testDecodedInterleavedFragment = fmp4.Parts{{
	SequenceNumber: 7,
	Tracks: []*fmp4.PartTrack{
		{
			ID:       1,
			BaseTime: 90000,
			Samples: []*fmp4.Sample{
				// the first sample points at the second half of the mdat.
				{Duration: 30, PTSOffset: -15, IsNonSyncSample: true, Payload: []byte{0xA3, 0xA4, 0xA5}},
				{Duration: 30, PTSOffset: 15, IsNonSyncSample: true, Payload: []byte{0xA0, 0xA1, 0xA2}},
			},
		},
		{
			ID:       2,
			BaseTime: 5000,
			Samples: []*fmp4.Sample{
				{Duration: 20, PTSOffset: 5, IsNonSyncSample: false, Payload: []byte{0xB0, 0xB1}},
				{Duration: 20, PTSOffset: -7, IsNonSyncSample: true, Payload: []byte{0xB2, 0xB3}},
			},
		},
	},
}}

// testEncodedConcatenatedParts stores two consecutive parts. The second moof is
// not at the beginning of the buffer, so its data offsets must be interpreted
// relative to its own start.
var testEncodedConcatenatedParts = []byte{
	0x00, 0x00, 0x00, 0x64, 0x6d, 0x6f, 0x6f, 0x66, 0x00, 0x00, 0x00, 0x10, 0x6d, 0x66, 0x68, 0x64,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x4c, 0x74, 0x72, 0x61, 0x66,
	0x00, 0x00, 0x00, 0x1c, 0x74, 0x66, 0x68, 0x64, 0x00, 0x00, 0x00, 0x38, 0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x1e, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x14,
	0x74, 0x66, 0x64, 0x74, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x14, 0x74, 0x72, 0x75, 0x6e, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02,
	0x00, 0x00, 0x00, 0x6c, 0x00, 0x00, 0x00, 0x0c, 0x6d, 0x64, 0x61, 0x74, 0x51, 0x52, 0x53, 0x54,
	0x00, 0x00, 0x00, 0x8c, 0x6d, 0x6f, 0x6f, 0x66, 0x00, 0x00, 0x00, 0x10, 0x6d, 0x66, 0x68, 0x64,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x74, 0x74, 0x72, 0x61, 0x66,
	0x00, 0x00, 0x00, 0x10, 0x74, 0x66, 0x68, 0x64, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x14, 0x74, 0x66, 0x64, 0x74, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x3c, 0x00, 0x00, 0x00, 0x24, 0x74, 0x72, 0x75, 0x6e, 0x01, 0x00, 0x0f, 0x01,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x94, 0x00, 0x00, 0x00, 0x14, 0x00, 0x00, 0x00, 0x03,
	0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xec, 0x00, 0x00, 0x00, 0x24, 0x74, 0x72, 0x75, 0x6e,
	0x01, 0x00, 0x0f, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x97, 0x00, 0x00, 0x00, 0x14,
	0x00, 0x00, 0x00, 0x02, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x6d, 0x64, 0x61, 0x74, 0x61, 0x62, 0x63, 0x64, 0x65,
}

var testDecodedConcatenatedParts = fmp4.Parts{
	{
		SequenceNumber: 1,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       1,
				BaseTime: 0,
				Samples: []*fmp4.Sample{
					{Duration: 30, PTSOffset: 0, IsNonSyncSample: false, Payload: []byte{0x51, 0x52}},
					{Duration: 30, PTSOffset: 0, IsNonSyncSample: false, Payload: []byte{0x53, 0x54}},
				},
			},
		},
	},
	{
		SequenceNumber: 2,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       1,
				BaseTime: 60,
				Samples: []*fmp4.Sample{
					{Duration: 20, PTSOffset: -20, IsNonSyncSample: false, Payload: []byte{0x61, 0x62, 0x63}},
					{Duration: 20, PTSOffset: 10, IsNonSyncSample: true, Payload: []byte{0x64, 0x65}},
				},
			},
		},
	},
}

func TestPartsDecodeHandwrittenFragments(t *testing.T) {
	cases := []struct {
		name string
		enc  []byte
		dec  fmp4.Parts
	}{
		{"interleaved tracks with default attributes", testEncodedInterleavedFragment, testDecodedInterleavedFragment},
		{"concatenated parts", testEncodedConcatenatedParts, testDecodedConcatenatedParts},
	}

	for _, ca := range cases {
		t.Run(ca.name, func(t *testing.T) {
			var parts fmp4.Parts
			err := parts.Unmarshal(ca.enc)
			require.NoError(t, err)
			require.Equal(t, ca.dec, parts)

			// every sample payload must live at the position the container
			// declares for it, and nowhere else across tracks.
			for _, part := range parts {
				for _, track := range part.Tracks {
					for _, sample := range track.Samples {
						require.NotEmpty(t, sample.Payload)
						require.True(t, bytes.Contains(ca.enc, sample.Payload))
					}
				}
			}

			// the fragment must decode identically when it is re-encoded.
			var buf seekablebuffer.Buffer
			err = parts.Marshal(&buf)
			require.NoError(t, err)

			var parts2 fmp4.Parts
			err = parts2.Unmarshal(buf.Bytes())
			require.NoError(t, err)
			require.Equal(t, parts, parts2)
		})
	}
}
