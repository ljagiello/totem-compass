"""Refit the right edge from both photos.

On the radio side only 1.4 mm of it shows (the USB-C and the JST cover the
rest), too short to fix its angle. The LED side shows it below the USB-C
(y -6.5 .. -2.5). Both sets of points, in board mm, are fitted together.
"""
import json
import numpy as np
import cv2

HERE = __file__.rsplit("/", 1)[0]
K, X0, Y1 = 40.0, -27.0, 27.0
g = cv2.cvtColor(cv2.imread(f"{HERE}/out/rect_led.png"), cv2.COLOR_BGR2GRAY).astype(float)
g = cv2.GaussianBlur(g, (0, 0), 1.5)
led = []
for y in np.arange(-6.5, -2.5, 0.1):
    v = int((Y1 - y) * K)
    u0, u1 = int((22.0 - X0) * K), int((24.5 - X0) * K)
    grad = np.gradient(g[v, u0:u1])
    i = int(np.argmax(grad))                      # dark board -> light table, going +x
    led.append([X0 + (u0 + i) / K, y])
led = np.array(led)
P = np.load(f"{HERE}/contour3_mm.npy")
m = (P[:, 0] > 22.3) & (P[:, 0] < 23.0) & (P[:, 1] > 7.9) & (P[:, 1] < 9.3)
radio = P[m]
Q = np.r_[led, radio]
c = Q.mean(0)
_, _, vt = np.linalg.svd(Q - c)
d = vt[0] if vt[0][1] > 0 else -vt[0]
r = (Q - c) @ vt[1]
ang = np.degrees(np.arctan2(d[1], d[0])) % 180
print(f"LED side x = {np.median(led[:,0]):.3f} (y -6.5..-2.5), radio side x = {np.median(radio[:,0]):.3f} (y 7.9..9.3)")
print(f"right edge: through ({c[0]:.3f}, {c[1]:.3f}) at {ang:.2f} deg, rms {np.sqrt(np.mean(r**2))*1000:.0f} um")
F = json.load(open(f"{HERE}/fits.json"))
F["right edge"].update({"p": c.tolist(), "d": d.tolist(), "ang": float(ang), "n": len(Q),
                        "rms": float(np.sqrt(np.mean(r ** 2))),
                        "source": "radio side y 7.9..9.3 plus LED side y -6.5..-2.5 (right_edge.py)"})
json.dump(F, open(f"{HERE}/fits.json", "w"), indent=1)
