// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

// Packed audio is how HLS carries audio with no container around it: ADTS
// AAC, MP3 and the like, segment after segment, each segment opening with
// an ID3 tag whose PRIV frame, owned by packedAudioOwner, holds the
// segment's timestamp, a 33-bit count of a 90 kHz clock. That tag is the
// only thing that places the audio on the playlist's timeline, and
// ffmpeg's readers of those streams do not read it: they start the audio
// at zero. A TS video beside it keeps its own timestamps through the join
// (-copyts), so the sound would be off the picture by as much as the
// stream's timeline is from zero: ten seconds, at one broadcaster. The
// join therefore gives such a part its start itself (-itsoffset).
const packedAudioOwner = "com.apple.streaming.transportStreamTimestamp"

// packedAudioHead is how much of a part is read for its tag, which comes
// first and is short: a few dozen bytes.
const packedAudioHead = 4 << 10

// packedAudioStart is where a packed audio part's timeline starts, in
// seconds, or zero for a part that is not packed audio.
func (f transferFiles) packedAudioStart(part string) float64 {
	file, err := f.open(part, os.O_RDONLY, 0)
	if err != nil {
		return 0
	}
	defer file.Close()
	head := make([]byte, packedAudioHead)
	n, _ := io.ReadFull(file, head)
	start, _ := packedAudioTimestamp(head[:n])
	return start
}

// packedAudioTimestamp reads the timestamp an ID3 tag at the head of a
// packed audio segment gives it, in seconds.
func packedAudioTimestamp(head []byte) (float64, bool) {
	if len(head) < 10 || string(head[:3]) != "ID3" || head[5]&0x40 != 0 {
		return 0, false // not ID3, or with an extended header, which HLS does not write
	}
	version := head[3]
	tag := head[10:min(len(head), 10+syncsafe(head[6:10]))]
	for len(tag) >= 10 && tag[0] != 0 {
		size := int(binary.BigEndian.Uint32(tag[4:8]))
		if version >= 4 {
			size = syncsafe(tag[4:8])
		}
		if size <= 0 || 10+size > len(tag) {
			break
		}
		frame := tag[10 : 10+size]
		if string(tag[:4]) == "PRIV" {
			owner, data, found := bytes.Cut(frame, []byte{0})
			if found && string(owner) == packedAudioOwner && len(data) >= 8 {
				return float64(binary.BigEndian.Uint64(data[:8])&(1<<33-1)) / 90000, true
			}
		}
		tag = tag[10+size:]
	}
	return 0, false
}

// syncsafe reads an ID3 size, seven bits to a byte.
func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}
