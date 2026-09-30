"""The ring in board mm: centre, radius, and the angular phase of its 60 LEDs.

ring.py found the LED bodies that are not lit or glaring (29 of 60); here
they go through led_reg.json into the board frame. A circle is fitted, and
since the LEDs sit every 6 degrees, the angles mod 6 give the phase.
"""
import json
import numpy as np

HERE = __file__.rsplit("/", 1)[0]
M = np.array(json.load(open(f"{HERE}/led_reg.json"))["S_board_to_px"])
px = np.array(json.load(open(f"{HERE}/ring_px.json"))["pts"])
B = (np.c_[px, np.ones(len(px))] @ np.linalg.inv(M).T)[:, :2]
A = np.c_[2 * B[:, 0], 2 * B[:, 1], np.ones(len(B))]
cx, cy, k = np.linalg.lstsq(A, (B ** 2).sum(1), rcond=None)[0]
R = np.sqrt(k + cx * cx + cy * cy)
d = np.hypot(B[:, 0] - cx, B[:, 1] - cy) - R
th = np.degrees(np.arctan2(B[:, 1] - cy, B[:, 0] - cx)) % 360
# circular mean of the phase within a 6-degree period
ph = np.degrees(np.angle(np.mean(np.exp(1j * np.radians(th * 60)))) / 60) % 6
dev = ((th - ph + 3) % 6) - 3
print(f"{len(B)} LEDs: centre ({cx:.3f}, {cy:.3f}) R {R:.3f} mm, radial rms {np.sqrt(np.mean(d**2))*1000:.0f} um")
print(f"phase {ph:.2f} deg (LEDs at {ph:.2f} + 6k), angular rms {np.sqrt(np.mean(dev**2)):.2f} deg "
      f"= {np.radians(np.sqrt(np.mean(dev**2)))*R*1000:.0f} um along the ring")
json.dump({"centre": [cx, cy], "R": R, "phase_deg": ph, "n_seen": len(B)},
          open(f"{HERE}/ring_board.json", "w"), indent=1)
