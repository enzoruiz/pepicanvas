import { describe, expect, it } from "vitest";

import { generateWav } from "./wav";

function ascii(view: DataView, offset: number, length: number): string {
  let text = "";
  for (let i = 0; i < length; i++)
    text += String.fromCharCode(view.getUint8(offset + i));
  return text;
}

describe("generateWav", () => {
  it("writes a 16-bit mono PCM header for the whole tone", () => {
    const wav = generateWav({ sampleRate: 8000, seconds: 2, frequency: 220 });
    const view = new DataView(wav);
    const dataLength = 8000 * 2 * 2;

    expect(wav.byteLength).toBe(44 + dataLength);
    expect(ascii(view, 0, 4)).toBe("RIFF");
    expect(view.getUint32(4, true)).toBe(36 + dataLength);
    expect(ascii(view, 8, 4)).toBe("WAVE");
    expect(ascii(view, 12, 4)).toBe("fmt ");
    expect(view.getUint32(16, true)).toBe(16);
    expect(view.getUint16(20, true)).toBe(1);
    expect(view.getUint16(22, true)).toBe(1);
    expect(view.getUint32(24, true)).toBe(8000);
    expect(view.getUint32(28, true)).toBe(16000);
    expect(view.getUint16(32, true)).toBe(2);
    expect(view.getUint16(34, true)).toBe(16);
    expect(ascii(view, 36, 4)).toBe("data");
    expect(view.getUint32(40, true)).toBe(dataLength);
  });

  it("writes a non-silent tone", () => {
    const view = new DataView(
      generateWav({ sampleRate: 8000, seconds: 1, frequency: 220 }),
    );
    let peak = 0;
    for (let offset = 44; offset < view.byteLength; offset += 2) {
      peak = Math.max(peak, Math.abs(view.getInt16(offset, true)));
    }

    expect(peak).toBeGreaterThan(0);
  });
});
