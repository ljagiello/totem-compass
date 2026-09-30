#!/bin/sh
# Re-run the whole photo measurement, in order. Needs python3 with numpy,
# scipy and opencv (cv2). Outputs land next to this script; overlays in out/.
set -e
cd "$(dirname "$0")"
P=${PYTHON:-python3}
rm -f padrows.json
# radio side: the ESP32's and the MAX-M10S's castellated pads, row by row
$P padrows.py 975 2165 1760 2165 15 10 bottom
$P padrows.py 820 960 815 2050 12 14 left
$P padrows.py 1970 985 1925 2075 12 14 right
$P padrows.py 872 2255 872 2825 10 9 m10l
$P padrows.py 1472 2255 1472 2835 10 9 m10r
$P padrows.py 1935 2140 1935 2430 10 4 tpl
$P padrows.py 2215 2150 2215 2440 10 4 tpr
$P homog.py          # ESP32 pads only: the starting point
$P homog2.py         # + MAX-M10S pads: the photo -> board-plane homography
$P frame2.py         # the board frame, from the left edge
$P fitall.py         # line and circle fits to the visible edge
$P outline2.py       # the outline
$P rectify.py        # out/rect_radio*.png
# LED side
$P edgescan.py photos/led.jpg e_llh.json row 300 600 2650 2900 25 1
$P edgescan.py photos/led.jpg e_lli.json col 2850 3150 560 760 25 -1
$P edgescan.py photos/led.jpg e_tab.json row 880 2050 600 760 25 -1
$P edgescan.py photos/led.jpg e_arc.json row 700 2150 2950 3400 25 1
$P edgescan.py photos/led.jpg e_lrh.json row 2300 2850 2550 2950 25 1
$P register_led2.py  # LED photo -> board frame
$P rectify_led.py    # out/rect_led*.png
$P ring.py           # ring LED centroids
$P ring_board.py     # ring centre, radius, phase in board mm
$P crystal.py        # the seven crystal LEDs
