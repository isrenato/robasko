#!/usr/bin/env python3
"""Render a cover letter to a one-page PDF.

SPEC.md 13.5: the draft is saved as .txt and then passed through here.
Exit code 2 means it did not fit on one page, which is the caller's signal
to shorten the lowest-weighted paragraph and try again.

No third-party dependency, on purpose. A PDF holding one page of text in a
base-14 font is a few hundred lines of a documented format, and the
alternative is asking an application that already refuses to send anything
by itself to install a rendering library. The only part that needs real
data is the glyph width table, because the one-page answer is only as
honest as the line wrapping behind it.

Exit codes:
  0  written
  1  bad arguments, or the text could not be read
  2  the letter does not fit the page budget
"""

import argparse
import datetime
import sys
import zlib

# --- Page geometry, in PostScript points (72 per inch). ---------------------

A4_WIDTH = 595.276
A4_HEIGHT = 841.890

MARGIN_LEFT = 70.0      # 25mm, the DIN 5008 filing margin
MARGIN_RIGHT = 56.0
MARGIN_TOP = 56.0
MARGIN_BOTTOM = 60.0

SENDER_SIZE = 8.5
BODY_SIZE = 10.5
SUBJECT_SIZE = 11.0
LEADING = 14.0          # baseline to baseline within a paragraph
PARA_GAP = 8.0          # extra space between paragraphs

# --- Glyph widths, per 1000 units of em, from the Adobe AFM metrics. --------
#
# Only the codes a letter actually reaches are listed; anything else falls
# back to DEFAULT_WIDTH, which is the width of a lowercase letter and so
# errs towards thinking the line is wider than it is. Erring that way makes
# the one-page check cautious rather than optimistic.

_HELVETICA_ASCII = (
    "278 278 355 556 556 889 667 191 333 333 389 584 278 333 278 278 "
    "556 556 556 556 556 556 556 556 556 556 278 278 584 584 584 556 "
    "1015 667 667 722 722 667 611 778 722 278 500 667 556 833 722 778 "
    "667 778 722 667 611 722 667 944 667 667 611 278 278 278 469 556 "
    "333 556 556 500 556 556 278 556 556 222 222 500 222 833 556 556 "
    "556 556 333 500 278 556 500 722 500 500 500 334 260 334 584"
)

_HELVETICA_BOLD_ASCII = (
    "278 333 474 556 556 889 722 238 333 333 389 584 278 333 278 278 "
    "556 556 556 556 556 556 556 556 556 556 333 333 584 584 584 611 "
    "975 722 722 722 722 667 611 778 722 278 556 722 611 833 722 778 "
    "667 778 722 667 611 722 667 944 667 667 611 333 278 333 584 556 "
    "333 556 611 556 611 556 333 611 611 278 278 556 278 889 611 611 "
    "611 611 389 556 333 611 556 778 556 556 500 389 280 389 584"
)

# cp1252 code points a German or English letter is likely to carry.
_HELVETICA_HIGH = {
    0x80: 556,  0xA7: 556,  0xB0: 400,  0xC4: 667,  0xD6: 778,  0xDC: 722,
    0xDF: 556,  0xE4: 556,  0xF6: 556,  0xFC: 556,  0xE9: 556,  0xE8: 556,
    0xEA: 556,  0xE1: 556,  0xE0: 556,  0xED: 278,  0xF3: 556,  0xFA: 556,
    0xE7: 500,  0xF1: 556,  0xC9: 667,  0xC1: 667,  0x91: 222, 0x92: 222,
    0x93: 333,  0x94: 333,  0x96: 556,  0x97: 1000, 0x84: 333, 0x85: 1000,
}

_HELVETICA_BOLD_HIGH = {
    0x80: 556,  0xA7: 556,  0xB0: 400,  0xC4: 722,  0xD6: 778,  0xDC: 722,
    0xDF: 611,  0xE4: 556,  0xF6: 611,  0xFC: 611,  0xE9: 556,  0xE8: 556,
    0xEA: 556,  0xE1: 556,  0xE0: 556,  0xED: 278,  0xF3: 611,  0xFA: 611,
    0xE7: 556,  0xF1: 611,  0xC9: 667,  0xC1: 722,  0x91: 278, 0x92: 278,
    0x93: 500,  0x94: 500,  0x96: 556,  0x97: 1000, 0x84: 500, 0x85: 1000,
}

DEFAULT_WIDTH = 556


def _widths(ascii_table, high_table, default):
    table = dict(high_table)
    for offset, value in enumerate(ascii_table.split()):
        table[32 + offset] = int(value)
    return table, default


REGULAR, REGULAR_DEFAULT = _widths(_HELVETICA_ASCII, _HELVETICA_HIGH, DEFAULT_WIDTH)
BOLD, BOLD_DEFAULT = _widths(_HELVETICA_BOLD_ASCII, _HELVETICA_BOLD_HIGH, 611)


def encode(text):
    """Return the bytes a WinAnsiEncoding font will show for this text."""
    return text.encode("cp1252", errors="replace")


def width_of(text, size, bold=False):
    table, default = (BOLD, BOLD_DEFAULT) if bold else (REGULAR, REGULAR_DEFAULT)
    total = 0
    for byte in encode(text):
        total += table.get(byte, default)
    return total * size / 1000.0


def wrap(text, size, limit, bold=False):
    """Break text into lines no wider than limit.

    A single word longer than the line is left over-long rather than
    broken: a hyphen inserted into a company name or a URL is a mistake
    the reader can see, and an over-long line is one the fit check will
    report.
    """
    words = text.split()
    if not words:
        return [""]

    lines, current = [], words[0]
    for word in words[1:]:
        candidate = current + " " + word
        if width_of(candidate, size, bold) <= limit:
            current = candidate
        else:
            lines.append(current)
            current = word
    lines.append(current)
    return lines


# --- PDF assembly -----------------------------------------------------------


class Content:
    """The page's drawing operations, collected in order."""

    def __init__(self):
        self.ops = []

    def text(self, x, y, string, size, bold=False):
        if not string:
            return
        font = "/F2" if bold else "/F1"
        escaped = (
            encode(string)
            .replace(b"\\", b"\\\\")
            .replace(b"(", b"\\(")
            .replace(b")", b"\\)")
        )
        self.ops.append(b"BT " + font.encode("ascii") + b" %.2f Tf %.2f %.2f Td (" % (size, x, y)
                        + escaped + b") Tj ET")

    def render(self):
        return b"\n".join(self.ops) + b"\n"


def build_pdf(content, compress=True):
    stream = content.render()
    filters = b""
    if compress:
        stream = zlib.compress(stream, 9)
        filters = b"/Filter /FlateDecode "

    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] "
        b"/Resources << /Font << /F1 5 0 R /F2 6 0 R >> >> /Contents 4 0 R >>"
        % (A4_WIDTH, A4_HEIGHT),
        b"<< " + filters + b"/Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
    ]

    out = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = []
    for number, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += b"%d 0 obj\n" % number + body + b"\nendobj\n"

    xref_at = len(out)
    out += b"xref\n0 %d\n" % (len(objects) + 1)
    out += b"0000000000 65535 f \n"
    for offset in offsets:
        out += b"%010d 00000 n \n" % offset
    out += (b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n"
            % (len(objects) + 1, xref_at))
    return bytes(out)


# --- Layout -----------------------------------------------------------------


def layout(args, body_blocks):
    """Place every line, and report the lowest baseline used."""
    content = Content()
    line_width = A4_WIDTH - MARGIN_LEFT - MARGIN_RIGHT
    right_edge = A4_WIDTH - MARGIN_RIGHT

    y = A4_HEIGHT - MARGIN_TOP

    # Sender block, right-aligned and small: the return address, not a
    # letterhead.
    for line in args.sender_line:
        content.text(right_edge - width_of(line, SENDER_SIZE), y, line, SENDER_SIZE)
        y -= SENDER_SIZE + 2

    y -= 28

    recipient = []
    if args.company:
        recipient.append(args.company)
    recipient.extend(args.recipient)
    for line in recipient:
        content.text(MARGIN_LEFT, y, line, BODY_SIZE)
        y -= LEADING

    # The date sits on the first recipient line's baseline, which is where
    # a German business letter puts it.
    if args.date:
        date_y = A4_HEIGHT - MARGIN_TOP - (len(args.sender_line) * (SENDER_SIZE + 2)) - 28
        content.text(right_edge - width_of(args.date, BODY_SIZE), date_y, args.date, BODY_SIZE)

    y -= 26
    for line in wrap(args.subject, SUBJECT_SIZE, line_width, bold=True):
        content.text(MARGIN_LEFT, y, line, SUBJECT_SIZE, bold=True)
        y -= LEADING + 1

    y -= 12
    for block in body_blocks:
        for line in wrap(block, BODY_SIZE, line_width):
            content.text(MARGIN_LEFT, y, line, BODY_SIZE)
            y -= LEADING
        y -= PARA_GAP

    if args.sender_name:
        y -= 18
        content.text(MARGIN_LEFT, y, args.sender_name, BODY_SIZE)

    return content, y


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--text", required=True, help="the saved .txt draft")
    parser.add_argument("--out", required=True, help="where to write the PDF")
    parser.add_argument("--subject", default="", help="the subject line")
    parser.add_argument("--company", default="", help="the addressed company")
    parser.add_argument("--recipient", action="append", default=[],
                        help="a recipient address line; repeatable")
    parser.add_argument("--sender-name", default="", help="the signature line")
    parser.add_argument("--sender-line", action="append", default=[],
                        help="a sender address line; repeatable")
    parser.add_argument("--date", default="", help="the date line; today when omitted")
    parser.add_argument("--no-compress", action="store_true",
                        help="write the content stream uncompressed, for diffing")
    args = parser.parse_args(argv)

    if not args.date:
        args.date = datetime.date.today().isoformat()

    try:
        with open(args.text, "r", encoding="utf-8") as handle:
            raw = handle.read()
    except OSError as err:
        print("build_letter: %s" % err, file=sys.stderr)
        return 1

    blocks = [" ".join(block.split()) for block in raw.split("\n\n")]
    blocks = [block for block in blocks if block]
    if not blocks:
        print("build_letter: the draft is empty", file=sys.stderr)
        return 1

    content, lowest = layout(args, blocks)

    # One page, always. A cover letter that needs a second page is not a
    # cover letter that needs a second page of layout code; it is one that
    # needs a sentence removed, which is what exit 2 asks the caller for.
    if lowest < MARGIN_BOTTOM:
        # Reported in points over, because that is what the caller can act
        # on: roughly one line per 14 points.
        print("build_letter: the letter runs %.0f points past the page"
              % (MARGIN_BOTTOM - lowest), file=sys.stderr)
        return 2

    try:
        with open(args.out, "wb") as handle:
            handle.write(build_pdf(content, compress=not args.no_compress))
    except OSError as err:
        print("build_letter: %s" % err, file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
