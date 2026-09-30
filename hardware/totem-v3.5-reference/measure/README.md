# Measuring the board from two photographs

Everything the PCB takes from the reference board — its outline, where the
parts sit, the ring and crystal geometry — is measured here from two
photographs, `photos/radio.jpg` (the ESP32/GNSS side) and `photos/led.jpg`
(the LED side). `run_all.sh` redoes all of it in order.

## The radio side

A phone photograph is not a scale drawing: this one has visible perspective
(the tab's top edge is 0.5 mm higher at one end while the sides are
vertical), and a single px-per-mm figure was off by up to 4% across the
board. So the photo is mapped to the board plane by a homography fitted to
features whose spacing is known exactly:

| Feature | Known | Fit |
| --- | --- | --- |
| ESP32-WROOM-32E castellated pads, 38 | 1.27 mm pitch, footprint positions | along-row residual 34 µm rms |
| MAX-M10S castellated pads, 18 | 1.10 mm pitch | spans 8.798 / 8.722 mm against 8.800 |

The fit uses only a pad's position along its row; across the row it only
asks the row to be straight, since where the sampling line was drawn is not
a measurement. TP4056 leads were tried and dropped: gull-wing shoulders put
their centroids 3-6% off.

The board frame is then fixed by the left edge (1600+ clean points), and
the edge is traced from the green mask and fitted stretch by stretch
(`fitall.py` lists the boxes and each fit's residual). Every edge keeps its
measured angle: an earlier version snapped near-orthogonal edges to the
axes, and the LED-side photo showed that moved the right side by up to
1.5 mm.

## The LED side

This photo is turned a quarter and seen from below: the USB-C's four shell
slots and two locating holes are along its bottom edge, the ESP32's antenna
overhang is off its left. Its 60-LED ring images round to 1%, so the photo
itself is close to a similarity. It is registered to the outline by the
edges that are clean from this side — the lower-left notch, the tab, the
lower-right notch — and the USB-C slots measured on the other side
(`register_led2.py` explains the two registrations and why the affine one
is kept).

## What is known how well

| Quantity | Uncertainty | Why |
| --- | --- | --- |
| Outline, left half and bottom | ±0.3 mm | fitted edges, 10-50 µm rms each; calibration within 1% |
| Outline, right edge and USB-C | ±1.3 mm in y | the two photos disagree by this much on the right side; each is self-consistent there. **Measure with calipers before ordering**: USB-C slot to lower-right notch, and the board's height at its right edge |
| Lower-right notch's outer corner | ±0.5 mm | under the JST on the radio side; its chamfer is taken from the LED side |
| Upper-left outside corner radius | ±0.5 mm | the u.FL cable crosses it |
| Ring centre and radius | ±0.15 mm | 29 LEDs, radial rms 122 µm |
| Ring start and direction | derived, not seen | the DOUT->DIN links are on an inner layer; see the hardware README |

Photos were converted from the originals to JPEG (quality 95); the
geometry is unchanged, individual numbers can move by a few µm on re-run.
