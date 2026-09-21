package emulator

import (
	"testing"

	"github.com/ljagiello/totem-compass/protocol"
)

// TestThePaletteMatchesTheProtocols: both packages carry the firmware's
// Colors table (project_data.py), and they have to agree — the CLI names
// a color from one and the board lights it from the other, so a
// correction applied to one and not the other is a Totem showing a
// color nobody asked for, with nothing in the build to say so.
//
// They are separate rather than shared on purpose: the emulator's copy is
// indexed directly by the LED code on the board, where ranging over a
// struct's values has been found to miscompile (see ParseColor). This
// test is what keeps them in step.
func TestThePaletteMatchesTheProtocols(t *testing.T) {
	if len(palette) != len(protocol.Palette) {
		t.Fatalf("the emulator holds %d colors, the protocol %d", len(palette), len(protocol.Palette))
	}
	for i := range palette {
		got, want := palette[i], protocol.Palette[i]
		if got.name != want.Name {
			t.Errorf("color %d is %q here and %q in the protocol", i, got.name, want.Name)
		}
		if got.rgb.R != want.RGB.R || got.rgb.G != want.RGB.G || got.rgb.B != want.RGB.B {
			t.Errorf("color %d (%s) is %v here and %v in the protocol", i, got.name, got.rgb, want.RGB)
		}
	}
}
