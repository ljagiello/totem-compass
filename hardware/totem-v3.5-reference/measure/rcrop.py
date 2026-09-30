"""rcrop.py IMG OUT x0 x1 y0 y1 [step] — crop a rectified board image (40 px/mm,
x from -27, y down from +27) by board mm, with a grid every `step` mm labelled."""
import sys
import numpy as np
import cv2

K, X0, Y1 = 40.0, -27.0, 27.0
src, out = sys.argv[1], sys.argv[2]
x0, x1, y0, y1 = (float(v) for v in sys.argv[3:7])
step = float(sys.argv[7]) if len(sys.argv) > 7 else 0.5
im = cv2.imread(src)
u0, u1 = int((x0 - X0) * K), int((x1 - X0) * K)
v0, v1 = int((Y1 - y1) * K), int((Y1 - y0) * K)
c = im[v0:v1, u0:u1].copy()
z = 1000 / max(c.shape[:2])
c = cv2.resize(c, None, fx=z, fy=z, interpolation=cv2.INTER_CUBIC)
for mm in np.arange(np.ceil(x0 / step) * step, x1, step):
    u = int((mm - x0) * K * z)
    major = abs(mm - round(mm)) < 1e-6
    cv2.line(c, (u, 0), (u, c.shape[0]), (0, 255, 255) if major else (0, 160, 255), 1)
    if major:
        cv2.putText(c, f"{mm:g}", (u + 2, 12), cv2.FONT_HERSHEY_SIMPLEX, 0.4, (0, 255, 255), 1)
for mm in np.arange(np.ceil(y0 / step) * step, y1, step):
    v = int((y1 - mm) * K * z)
    major = abs(mm - round(mm)) < 1e-6
    cv2.line(c, (0, v), (c.shape[1], v), (0, 255, 255) if major else (0, 160, 255), 1)
    if major:
        cv2.putText(c, f"{mm:g}", (2, v - 2), cv2.FONT_HERSHEY_SIMPLEX, 0.4, (0, 255, 255), 1)
cv2.imwrite(out, c, [cv2.IMWRITE_JPEG_QUALITY, 90])
print(out, c.shape[1], c.shape[0])
