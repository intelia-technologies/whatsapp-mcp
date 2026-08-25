package whatsapp

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
)

func requireAudioTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(mediaExecutable(name)); err != nil {
			t.Skipf("%s is required for voice-note conversion tests", name)
		}
	}
}

func testWAV(sampleRate, seconds int) []byte {
	sampleCount := sampleRate * seconds
	dataSize := sampleCount * 2
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(36+dataSize))
	wav.WriteString("WAVEfmt ")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(2))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(16))
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(dataSize))
	for i := range sampleCount {
		sample := int16((i%100)-50) * 200
		_ = binary.Write(&wav, binary.LittleEndian, sample)
	}
	return wav.Bytes()
}

func TestConvertAudioToVoiceNote(t *testing.T) {
	requireAudioTools(t)

	voiceData, duration, err := convertAudioToVoiceNote(context.Background(), testWAV(8000, 1))
	if err != nil {
		t.Fatalf("convertAudioToVoiceNote returned an error: %v", err)
	}
	if !bytes.HasPrefix(voiceData, []byte("OggS")) {
		t.Fatalf("converted audio is not an Ogg stream: header %q", voiceData[:4])
	}
	if duration != 1 {
		t.Fatalf("duration = %d, want 1", duration)
	}
}

func TestConvertAudioToVoiceNoteRejectsInvalidAudio(t *testing.T) {
	requireAudioTools(t)

	if _, _, err := convertAudioToVoiceNote(context.Background(), []byte("not audio")); err == nil {
		t.Fatal("convertAudioToVoiceNote accepted invalid audio")
	}
}
