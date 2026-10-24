// A generated WAV tone, so the media commands play a local source that is
// fully loaded before measurement and never involves an upload (S3).

export interface ToneOptions {
  sampleRate: number;
  seconds: number;
  frequency: number;
}

const headerBytes = 44;
const amplitude = 0.1;

export function generateWav({
  sampleRate,
  seconds,
  frequency,
}: ToneOptions): ArrayBuffer {
  const samples = Math.round(sampleRate * seconds);
  const dataBytes = samples * 2;
  const buffer = new ArrayBuffer(headerBytes + dataBytes);
  const view = new DataView(buffer);
  const ascii = (offset: number, text: string) => {
    for (let i = 0; i < text.length; i++) {
      view.setUint8(offset + i, text.charCodeAt(i));
    }
  };
  ascii(0, "RIFF");
  view.setUint32(4, 36 + dataBytes, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, "data");
  view.setUint32(40, dataBytes, true);
  for (let i = 0; i < samples; i++) {
    const value = Math.sin((2 * Math.PI * frequency * i) / sampleRate);
    view.setInt16(
      headerBytes + i * 2,
      Math.round(value * amplitude * 32767),
      true,
    );
  }
  return buffer;
}
