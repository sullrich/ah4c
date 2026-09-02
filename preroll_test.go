package main

import (
	"strings"
	"testing"
)

func testPrerollProbe(codec, rate, audio string) prerollProbe {
	var p prerollProbe
	p.Streams = append(p.Streams, struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		RFrameRate string `json:"r_frame_rate"`
	}{CodecType: "video", CodecName: codec, RFrameRate: rate})
	if audio != "" {
		p.Streams = append(p.Streams, struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			RFrameRate string `json:"r_frame_rate"`
		}{CodecType: "audio", CodecName: audio})
	}
	return p
}

func TestPlanPrerollEncodesH264AsHEVC(t *testing.T) {
	t.Setenv("ENCODER_CODEC", "h265")
	plan, err := planPreroll("preroll.mp4", testPrerollProbe("h264", "30000/1001", "aac"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.args, "\x00")
	if !strings.Contains(args, "-c:v\x00libx265") {
		t.Fatalf("ffmpeg args do not select libx265: %v", plan.args)
	}
	if strings.Contains(args, "-c:v\x00copy") {
		t.Fatalf("H.264 video was copied instead of converted: %v", plan.args)
	}
	if plan.kind != "H.264 video converted to H.265, AAC audio copied" {
		t.Fatalf("pre-roll description = %q", plan.kind)
	}
}

func TestPlanPrerollCopiesHEVCForHEVCEncoder(t *testing.T) {
	t.Setenv("ENCODER_CODEC", "h265")
	plan, err := planPreroll("preroll.ts", testPrerollProbe("hevc", "30/1", ""))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.args, "\x00")
	if !strings.Contains(args, "-c:v\x00copy") {
		t.Fatalf("H.265 video was not copied: %v", plan.args)
	}
	if strings.Contains(args, "libx265") {
		t.Fatalf("H.265 video was unnecessarily converted: %v", plan.args)
	}
	if !strings.Contains(plan.kind, "H.265 video copied without conversion") {
		t.Fatalf("pre-roll description = %q", plan.kind)
	}
}
