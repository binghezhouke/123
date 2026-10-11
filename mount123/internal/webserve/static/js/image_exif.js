// Small, dependency-free JPEG EXIF reader used by the gallery. It only reads
// the APP1 prefix, so showing metadata never requires a second network read.
(() => {
    const labels = new Map([
        [0x010f, '制造商'], [0x0110, '型号'], [0x0132, '拍摄时间'],
        [0x9003, '原始拍摄时间'], [0x8827, 'ISO'], [0x829a, '曝光时间'],
        [0x829d, '光圈'], [0x920a, '焦距'], [0xa002, '宽度'], [0xa003, '高度'],
    ]);
    const read = async blob => {
        if (!blob || !/^image\/jpe?g$/i.test(blob.type || '')) return null;
        const bytes = new DataView(await blob.slice(0, 2 * 1024 * 1024).arrayBuffer());
        if (bytes.byteLength < 4 || bytes.getUint16(0) !== 0xffd8) return null;
        let offset = 2;
        while (offset + 4 <= bytes.byteLength) {
            if (bytes.getUint8(offset) !== 0xff) break;
            const marker = bytes.getUint8(offset + 1); offset += 2;
            if (marker === 0xda || marker === 0xd9) break;
            const length = bytes.getUint16(offset); if (length < 2 || offset + length > bytes.byteLength) break;
            if (marker === 0xe1 && length >= 8 && ascii(bytes, offset + 2, 6) === 'Exif\0\0') {
                return parseTiff(bytes, offset + 8);
            }
            offset += length;
        }
        return null;
    };
    function ascii(view, offset, length) { let value = ''; for (let i = 0; i < length && offset + i < view.byteLength; i++) value += String.fromCharCode(view.getUint8(offset + i)); return value; }
    function parseTiff(view, start) {
        if (start + 8 > view.byteLength) return null;
        const little = ascii(view, start, 2) === 'II';
        if (!little && ascii(view, start, 2) !== 'MM') return null;
        const u16 = offset => view.getUint16(start + offset, little);
        const u32 = offset => view.getUint32(start + offset, little);
        if (u16(2) !== 42) return null;
        const output = {};
        const seen = new Set();
        const ifd = offset => {
            if (!Number.isFinite(offset) || seen.has(offset) || start + offset + 2 > view.byteLength) return;
            seen.add(offset);
            const count = u16(offset); const base = offset + 2;
            for (let i = 0; i < count && start + base + i * 12 + 12 <= view.byteLength; i++) {
                const at = base + i * 12, tag = u16(at), type = u16(at + 2), number = u32(at + 4);
                if (tag === 0x8769 || tag === 0x8825) { ifd(u32(at + 8)); continue; }
                const value = valueAt(at + 8, type, number);
                if (value != null && labels.has(tag)) output[labels.get(tag)] = format(tag, value);
            }
        };
        const valueAt = (at, type, number) => {
            const sizes = {1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1};
            const size = sizes[type]; if (!size || !number || number > 1000) return null;
            const bytes = size * number, position = bytes <= 4 ? at : start + u32(at);
            if (position + bytes > view.byteLength) return null;
            if (type === 2) return ascii(view, position, number).replace(/\0+$/, '').trim();
            if (type === 3) return number === 1 ? u16(at) : Array.from({length: number}, (_, i) => view.getUint16(position + i * 2, little));
            if (type === 4) return number === 1 ? view.getUint32(position, little) : null;
            if (type === 5) { const n = view.getUint32(position, little), d = view.getUint32(position + 4, little); return d ? n / d : null; }
            return null;
        };
        const format = (tag, value) => {
            if (tag === 0x829a) return value < 1 ? `1/${Math.round(1 / value)} s` : `${value.toFixed(2)} s`;
            if (tag === 0x829d) return `f/${Number(value).toFixed(1)}`;
            if (tag === 0x920a) return `${Number(value).toFixed(1)} mm`;
            if (tag === 0x8827 && Array.isArray(value)) return String(value[0]);
            return String(value);
        };
        ifd(u32(4));
        return Object.keys(output).length ? output : null;
    }
    globalThis.readImageExif = read;
})();
