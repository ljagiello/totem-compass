"""pxgrid.py SRC OUT x0 y0 x1 y1 step [maxdim] — crop with a full-resolution pixel grid."""
import sys
import cv2

src, out = sys.argv[1], sys.argv[2]
x0, y0, x1, y1, step = (int(v) for v in sys.argv[3:8])
m = int(sys.argv[8]) if len(sys.argv) > 8 else 1000
im = cv2.imread(src)[y0:y1, x0:x1].copy()
s = min(1.0, m / max(im.shape[:2]))
im = cv2.resize(im, None, fx=s, fy=s, interpolation=cv2.INTER_AREA)
for gx in range((x0 // step + 1) * step, x1, step):
    X = int((gx - x0) * s)
    cv2.line(im, (X, 0), (X, im.shape[0]), (0, 255, 255), 1)
    cv2.putText(im, str(gx), (X + 2, 12), cv2.FONT_HERSHEY_SIMPLEX, 0.38, (0, 255, 255), 1)
for gy in range((y0 // step + 1) * step, y1, step):
    Y = int((gy - y0) * s)
    cv2.line(im, (0, Y), (im.shape[1], Y), (0, 255, 255), 1)
    cv2.putText(im, str(gy), (2, Y - 2), cv2.FONT_HERSHEY_SIMPLEX, 0.38, (0, 255, 255), 1)
cv2.imwrite(out, im, [cv2.IMWRITE_JPEG_QUALITY, 90])
print(out, im.shape[1], im.shape[0])
