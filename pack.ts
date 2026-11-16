import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { readObject, writeObject, hashObject, parseCommit, parseTree, type ObjectType } from './gitstore.ts';

type GitObject = { type: ObjectType; data: Buffer };
type Objects = Map<string, GitObject>;
export type PktItem = { type: 'line'; data: Buffer } | { type: 'flush' | 'delim' | 'response-end' };

export function pktLine(payload: string | Buffer): Buffer {
  const data = Buffer.isBuffer(payload) ? payload : Buffer.from(payload);
  const length = data.length + 4;
  if (length > 0xffff) throw new Error('Pkt-line too long');
  return Buffer.concat([Buffer.from(length.toString(16).padStart(4, '0')), data]);
}
export const pktFlush = (): Buffer => Buffer.from('0000');
export const pktDelim = (): Buffer => Buffer.from('0001');
export const pktResponseEnd = (): Buffer => Buffer.from('0002');
export function parsePktLines(buf: Buffer, offset = 0): PktItem[] {
  if (!Number.isInteger(offset) || offset < 0 || offset > buf.length) throw new Error('Invalid pkt-line offset');
  const items: PktItem[] = [];
  while (offset < buf.length) {
    const prefix = buf.subarray(offset, offset + 4).toString('latin1');
    if (offset + 4 > buf.length || !/^[0-9a-fA-F]{4}$/.test(prefix)) throw new Error('Malformed pkt-line length');
    const length = parseInt(prefix, 16);
    offset += 4;
    if (length <= 2) items.push({ type: (['flush', 'delim', 'response-end'] as const)[length] });
    else {
      if (length < 4 || offset + length - 4 > buf.length) throw new Error('Malformed pkt-line length');
      items.push({ type: 'line', data: buf.subarray(offset, offset + length - 4) });
      offset += length - 4;
    }
  }
  return items;
}

const typeNames: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const typeCodes: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const digest = (data: Buffer): Buffer => createHash('sha1').update(data).digest();

export function buildPack(shas: string[], objects: Objects): Buffer {
  const unique = [...new Set(shas)];
  const header = Buffer.alloc(12);
  header.write('PACK'); header.writeUInt32BE(2, 4); header.writeUInt32BE(unique.length, 8);
  const chunks = [header];
  for (const sha of unique) {
    const object = objects.get(sha);
    if (!object) throw new Error(`Missing object ${sha}`);
    let size = object.data.length;
    const bytes = [(typeCodes[object.type] << 4) | (size % 16)];
    size = Math.floor(size / 16);
    while (size) {
      bytes[bytes.length - 1] |= 0x80;
      bytes.push(size % 128); size = Math.floor(size / 128);
    }
    chunks.push(Buffer.from(bytes), deflateSync(object.data));
  }
  const body = Buffer.concat(chunks);
  return Buffer.concat([body, digest(body)]);
}

export function inflateExact(buf: Buffer, start: number): { data: Buffer; next: number } {
  if (!Number.isInteger(start) || start < 0 || start >= buf.length) throw new Error('Invalid compressed stream offset');
  // Zlib reports how many input bytes the first complete stream consumed,
  // excluding subsequent pack entries. Keep its normal strict truncation checks.
  const result = inflateSync(buf.subarray(start), { info: true });
  return { data: result.buffer, next: start + result.engine.bytesWritten };
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const byte = (): number => {
    if (pos >= delta.length) throw new Error('Truncated delta');
    return delta[pos++];
  };
  const size = (): number => {
    let value = 0, factor = 1, b: number;
    do {
      b = byte(); value += (b & 127) * factor;
      if (!Number.isSafeInteger(value)) throw new Error('Delta size overflow');
      factor *= 128;
    } while (b & 128);
    return value;
  };
  if (size() !== base.length) throw new Error('Delta base size mismatch');
  const targetSize = size(), chunks: Buffer[] = [];
  let total = 0;
  while (pos < delta.length) {
    const op = byte();
    let chunk: Buffer;
    if (op & 128) {
      let offset = 0, length = 0;
      for (let i = 0; i < 4; i++) if (op & (1 << i)) offset += byte() * 2 ** (8 * i);
      for (let i = 0; i < 3; i++) if (op & (1 << (4 + i))) length += byte() * 2 ** (8 * i);
      if (!length) length = 0x10000;
      if (offset + length > base.length) throw new Error('Delta copy outside base');
      chunk = base.subarray(offset, offset + length);
    } else {
      if (!op || pos + op > delta.length) throw new Error('Invalid delta insert');
      chunk = delta.subarray(pos, pos + op); pos += op;
    }
    total += chunk.length;
    if (total > targetSize) throw new Error('Delta exceeds target size');
    chunks.push(chunk);
  }
  if (total !== targetSize) throw new Error('Delta target size mismatch');
  return Buffer.concat(chunks, total);
}

export function parsePack(buf: Buffer, objects: Objects): { shas: string[]; count: number } {
  if (buf.length < 32 || buf.subarray(0, 4).toString() !== 'PACK') throw new Error('Invalid pack header');
  const version = buf.readUInt32BE(4), count = buf.readUInt32BE(8);
  if (version !== 2 && version !== 3) throw new Error('Unsupported pack version');
  const end = buf.length - 20;
  if (!digest(buf.subarray(0, end)).equals(buf.subarray(end))) throw new Error('Pack checksum mismatch');
  let pos = 12;
  const byte = (): number => { if (pos >= end) throw new Error('Truncated pack'); return buf[pos++]; };
  const offsets = new Map<number, GitObject>(), resolved = new Map<string, GitObject>();
  const shas: string[] = [];
  for (let i = 0; i < count; i++) {
    const entryOffset = pos;
    let b = byte(), size = b & 15, factor = 16;
    const code = (b >> 4) & 7;
    while (b & 128) {
      b = byte(); size += (b & 127) * factor; factor *= 128;
      if (!Number.isSafeInteger(size)) throw new Error('Pack size overflow');
    }
    let base: GitObject | undefined;
    if (code === 6) {
      b = byte(); let distance = b & 127;
      while (b & 128) {
        b = byte(); distance = (distance + 1) * 128 + (b & 127);
        if (!Number.isSafeInteger(distance)) throw new Error('Pack offset overflow');
      }
      base = offsets.get(entryOffset - distance);
      if (!base) throw new Error('Missing OFS_DELTA base');
    } else if (code === 7) {
      if (pos + 20 > end) throw new Error('Truncated REF_DELTA');
      const sha = buf.subarray(pos, pos + 20).toString('hex'); pos += 20;
      base = resolved.get(sha) ?? objects.get(sha);
      if (!base) throw new Error(`Missing REF_DELTA base ${sha}`);
    } else if (!typeNames[code]) throw new Error('Invalid pack object type');
    const inflated = inflateExact(buf.subarray(0, end), pos); pos = inflated.next;
    if (inflated.data.length !== size) throw new Error('Pack object size mismatch');
    const object: GitObject = base ? { type: base.type, data: applyDelta(base.data, inflated.data) }
      : { type: typeNames[code], data: inflated.data };
    const sha = hashObject(object.type, object.data);
    offsets.set(entryOffset, object); resolved.set(sha, object); shas.push(sha);
  }
  if (pos !== end) throw new Error('Unexpected data after pack entries');
  for (const [sha, object] of resolved) {
    writeObject(object.type, object.data);
    objects.set(sha, { type: object.type, data: Buffer.from(object.data) });
  }
  return { shas, count };
}

function tagTarget(data: Buffer): string {
  const match = /^object ([0-9a-f]{40})$/m.exec(data.toString());
  if (!match) throw new Error('Invalid tag target');
  return match[1];
}

export function depthMap(wants: string[], maxDepth = Infinity): Map<string, number> {
  const depths = new Map<string, number>(), tags = new Set<string>();
  const queue: Array<[string, number]> = wants.map(sha => [sha, 1]);
  for (let i = 0; i < queue.length; i++) {
    const [sha, depth] = queue[i], object = readObject(sha);
    if (!object) continue;
    if (object.type === 'tag') {
      if (!tags.has(sha)) { tags.add(sha); queue.push([tagTarget(object.data), depth]); }
      continue;
    }
    if (object.type !== 'commit' || (depths.get(sha) ?? Infinity) <= depth) continue;
    depths.set(sha, depth);
    if (depth < maxDepth) for (const parent of parseCommit(object.data).parents) queue.push([parent, depth + 1]);
  }
  return depths;
}
export function ancestorsOf(tips: string[]): Set<string> { return new Set(depthMap(tips).keys()); }
export function isAncestor(maybeAncestor: string, tip: string): boolean {
  const queue = [tip], visited = new Set<string>();
  for (let i = 0; i < queue.length; i++) {
    const sha = queue[i];
    if (visited.has(sha)) continue;
    visited.add(sha);
    const object = readObject(sha);
    if (object?.type === 'tag') queue.push(tagTarget(object.data));
    else if (object?.type === 'commit') {
      if (sha === maybeAncestor) return true;
      queue.push(...parseCommit(object.data).parents);
    }
  }
  return false;
}
export function shallowBoundary(wants: string[], depth: number): Set<string> {
  const boundary = new Set<string>();
  for (const [sha, distance] of depthMap(wants, depth)) {
    if (distance === depth && parseCommit(readObject(sha)!.data).parents.length) boundary.add(sha);
  }
  return boundary;
}

export function collectFetchObjects(wants: string[], haves: Set<string>, boundary: Set<string>, haveBoundary = new Set<string>()): string[] {
  const excluded = new Set<string>(), haveQueue = [...haves];
  for (let i = 0; i < haveQueue.length; i++) {
    const sha = haveQueue[i];
    if (excluded.has(sha)) continue;
    excluded.add(sha);
    const object = readObject(sha);
    if (object?.type === 'commit' && !haveBoundary.has(sha)) haveQueue.push(...parseCommit(object.data).parents);
  }
  const included = new Set<string>(), trees: string[] = [];
  const queue = [...wants], visited = new Set<string>();
  for (let i = 0; i < queue.length; i++) {
    const sha = queue[i];
    if (visited.has(sha)) continue;
    visited.add(sha);
    const object = readObject(sha);
    if (!object) throw new Error(`Missing object ${sha}`);
    if (object.type === 'commit') {
      if (excluded.has(sha)) continue;
      included.add(sha);
      const commit = parseCommit(object.data); trees.push(commit.tree);
      if (!boundary.has(sha)) queue.push(...commit.parents);
    } else if (object.type === 'tag') {
      included.add(sha); queue.push(tagTarget(object.data));
    } else trees.push(sha);
  }
  for (let i = 0; i < trees.length; i++) {
    const sha = trees[i];
    if (included.has(sha)) continue;
    const object = readObject(sha);
    if (!object) throw new Error(`Missing object ${sha}`);
    included.add(sha);
    if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) if (entry.mode !== '160000') trees.push(entry.sha);
    }
  }
  return [...included];
}
