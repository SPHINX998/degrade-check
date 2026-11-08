"""Draws icon.ico: a magnifier over a falling line, for "degradation check".

Needs Pillow. After changing it, rebuild the Windows resource from backend/:
    rsrc -ico cmd/relaycheck-client/icon.ico -manifest cmd/relaycheck-client/app.manifest
         -arch amd64 -o cmd/relaycheck-client/rsrc_windows_amd64.syso
"""
import pathlib

from PIL import Image, ImageDraw

CANVAS = 1024
HERE = pathlib.Path(__file__).parent


def draw():
    image = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    pen = ImageDraw.Draw(image)
    # Background: a blue that fades a little downward.
    gradient = Image.new("RGBA", (CANVAS, CANVAS))
    for y in range(CANVAS):
        t = y / CANVAS
        color = (int(37 + 12 * t), int(99 - 42 * t), int(235 - 60 * t), 255)
        ImageDraw.Draw(gradient).line([(0, y), (CANVAS, y)], fill=color)
    mask = Image.new("L", (CANVAS, CANVAS), 0)
    ImageDraw.Draw(mask).rounded_rectangle((32, 32, CANVAS - 32, CANVAS - 32), radius=210, fill=255)
    image.paste(gradient, (0, 0), mask)

    white = (255, 255, 255, 255)
    # Magnifier: lens, then handle.
    center, radius, ring = (440, 430), 262, 66
    pen.ellipse((center[0] - radius, center[1] - radius, center[0] + radius, center[1] + radius), fill=(255, 255, 255, 44), outline=white, width=ring)
    pen.line([(642, 632), (806, 796)], fill=white, width=98)
    pen.ellipse((757, 747, 855, 845), fill=white)
    pen.ellipse((593, 583, 691, 681), fill=white)

    # The falling line inside the lens, with its arrowhead.
    red = (255, 112, 92, 255)
    points = [(292, 338), (402, 440), (470, 382), (578, 508)]
    pen.line(points, fill=red, width=52, joint="curve")
    for x, y in points[:3]:
        pen.ellipse((x - 26, y - 26, x + 26, y + 26), fill=red)
    pen.polygon([(610, 546), (610, 424), (494, 546)], fill=red)
    return image


source = draw()
source.save(HERE / "icon.ico", sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
print("icon.ico", (HERE / "icon.ico").stat().st_size)
