/**
 * Decoding helpers for the outbound desktop identity headers captured in
 * request log attempt details.
 *
 * x-oai-attestation carries `{"v":1,"s":0,"t":"v1.<base64url CBOR>"}`. The CBOR
 * layer is intentionally minimal (uint / negative / bytes / text / array /
 * map / float), matching the encoder implemented in the gateway; anything
 * outside that subset is rejected.
 */

import type { OutboundIdentityHeadersDto } from '@/app/resources/request-logs'

export interface OutboundHeaderRow {
  name: string
  value: string
  /** Preformatted popup text shown on hover when the value is decodable. */
  decoded?: string
}

export interface DecodedAttestation {
  bundleId: string
  errorCode: number
  schemaVersion: number
  languages: string[]
  locale: string
  timezone: string
  screenSizeSum: number
  screenScale: number
  appSessionId: string
}

const maxHeaderValueBytes = 8192
const maxDecodedBytes = 64 * 1024

export function outboundHeaderRows(headers: OutboundIdentityHeadersDto): OutboundHeaderRow[] {
  const values: Array<[string, string]> = [
    ['user-agent', headers['user-agent'] ?? ''],
    ['originator', headers.originator ?? ''],
    ['x-oai-attestation', headers['x-oai-attestation'] ?? ''],
    ['x-codex-window-id', headers['x-codex-window-id'] ?? ''],
    ['x-codex-turn-metadata', headers['x-codex-turn-metadata'] ?? ''],
  ]
  return values.map(([name, value]) => {
    const row: OutboundHeaderRow = { name, value }
    if (name === 'x-oai-attestation' && value) {
      const decoded = describeAttestation(value)
      if (decoded) row.decoded = decoded
    } else if (name === 'x-codex-turn-metadata' && value) {
      const formatted = formatTurnMetadata(value)
      if (formatted) row.decoded = formatted
    }
    return row
  })
}

export function describeAttestation(raw: string): string | null {
  const decoded = decodeAttestation(raw)
  if (!decoded) return null
  const lines = [
    `bundle_id: ${decoded.bundleId}`,
    `error_code: ${decoded.errorCode}`,
    `schemaVersion: ${decoded.schemaVersion}`,
    `languages: ${decoded.languages.join(', ')}`,
    `locale: ${decoded.locale}`,
    `timezone: ${decoded.timezone}`,
    `screenSizeSum: ${decoded.screenSizeSum}`,
    `screenScale: ${decoded.screenScale}`,
    `appSessionId: ${decoded.appSessionId}`,
  ]
  return lines.join('\n')
}

export function formatTurnMetadata(raw: string): string | null {
  if (raw.length > maxHeaderValueBytes) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null
  return JSON.stringify(parsed, null, 2)
}

export function decodeAttestation(raw: string): DecodedAttestation | null {
  if (raw.length > maxHeaderValueBytes) return null
  let envelope: unknown
  try {
    envelope = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof envelope !== 'object' || envelope === null) return null
  const record = envelope as Record<string, unknown>
  if (record.v !== 1 || record.s !== 0 || typeof record.t !== 'string') return null
  const token = record.t
  if (!token.startsWith('v1.')) return null
  const payload = decodeBase64Url(token.slice('v1.'.length))
  if (!payload) return null
  try {
    const outer = decodeCbor(payload)
    if (!isPlainRecord(outer)) return null
    const bundleId = outer['bundle_id']
    const errorCode = outer['error_code']
    const signals = outer['f']
    if (typeof bundleId !== 'string') return null
    if (typeof errorCode !== 'number' || !Number.isInteger(errorCode)) return null
    if (!(signals instanceof Uint8Array)) return null
    const inner = decodeCbor(signals)
    if (!isPlainRecord(inner)) return null
    const schemaVersion = inner[0]
    const languages = inner[1]
    const locale = inner[2]
    const timezone = inner[3]
    const screenSizeSum = inner[4]
    const screenScale = inner[5]
    const appSessionId = inner[6]
    if (typeof schemaVersion !== 'number' || !Number.isInteger(schemaVersion)) return null
    if (!Array.isArray(languages) || !languages.every((entry) => typeof entry === 'string')) {
      return null
    }
    if (typeof locale !== 'string' || typeof timezone !== 'string') return null
    if (typeof screenSizeSum !== 'number' || !Number.isSafeInteger(screenSizeSum)) return null
    if (typeof screenScale !== 'number' || !Number.isFinite(screenScale)) return null
    if (typeof appSessionId !== 'string') return null
    return {
      bundleId,
      errorCode,
      schemaVersion,
      languages,
      locale,
      timezone,
      screenSizeSum,
      screenScale,
      appSessionId,
    }
  } catch {
    return null
  }
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function decodeBase64Url(encoded: string): Uint8Array | null {
  if (encoded.length === 0 || encoded.length > maxDecodedBytes) return null
  let base64 = encoded.replace(/-/g, '+').replace(/_/g, '/')
  while (base64.length % 4 !== 0) base64 += '='
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(base64)) return null
  let binary: string
  try {
    binary = atob(base64)
  } catch {
    return null
  }
  const bytes = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index++) {
    bytes[index] = binary.charCodeAt(index)
  }
  return bytes
}

class CborReader {
  private readonly bytes: Uint8Array
  private offset = 0

  consumed(): number {
    return this.offset
  }

  constructor(bytes: Uint8Array) {
    this.bytes = bytes
  }

  private readByte(): number {
    if (this.offset >= this.bytes.length) throw new Error('cbor: unexpected end')
    return this.bytes[this.offset++]
  }

  private readArgument(additional: number): number {
    switch (additional) {
      case 24:
        return this.readByte()
      case 25:
        return (this.readByte() << 8) | this.readByte()
      case 26: {
        const value =
          (this.readByte() << 24) |
          (this.readByte() << 16) |
          (this.readByte() << 8) |
          this.readByte()
        return value >>> 0
      }
      case 27: {
        const high =
          (this.readByte() << 24) |
          (this.readByte() << 16) |
          (this.readByte() << 8) |
          this.readByte()
        const low =
          (this.readByte() << 24) |
          (this.readByte() << 16) |
          (this.readByte() << 8) |
          this.readByte()
        const value = BigInt(high >>> 0) * 0x1_0000_0000n + BigInt(low >>> 0)
        if (value > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('cbor: integer overflow')
        return Number(value)
      }
      default:
        throw new Error('cbor: unsupported additional information')
    }
  }

  private readLength(additional: number): number {
    if (additional === 31) throw new Error('cbor: indefinite length is unsupported')
    return this.readArgument(additional)
  }

  private readFloat(additional: number): number {
    const view = new DataView(
      this.bytes.buffer,
      this.bytes.byteOffset + this.offset,
      this.bytes.byteLength - this.offset,
    )
    switch (additional) {
      case 25: {
        if (this.bytes.length - this.offset < 2) throw new Error('cbor: unexpected end')
        const half = view.getUint16(0, false)
        this.offset += 2
        const exponent = (half >> 10) & 0x1f
        const fraction = half & 0x3ff
        const sign = half & 0x8000 ? -1 : 1
        if (exponent === 0) return sign * 2 ** -14 * (fraction / 1024)
        if (exponent === 31) return fraction === 0 ? sign * Infinity : NaN
        return sign * 2 ** (exponent - 15) * (1 + fraction / 1024)
      }
      case 26: {
        if (this.bytes.length - this.offset < 4) throw new Error('cbor: unexpected end')
        const value = view.getFloat32(0, false)
        this.offset += 4
        return value
      }
      case 27: {
        if (this.bytes.length - this.offset < 8) throw new Error('cbor: unexpected end')
        const value = view.getFloat64(0, false)
        this.offset += 8
        return value
      }
      default:
        throw new Error('cbor: unsupported float width')
    }
  }

  read(): unknown {
    const head = this.readByte()
    const major = head >> 5
    const additional = head & 0x1f
    switch (major) {
      case 0:
        return this.readArgument(additional)
      case 1: {
        const value = this.readArgument(additional)
        return -1 - value
      }
      case 2: {
        const length = this.readLength(additional)
        if (length > this.bytes.length - this.offset) throw new Error('cbor: unexpected end')
        const bytes = this.bytes.slice(this.offset, this.offset + length)
        this.offset += length
        return bytes
      }
      case 3: {
        const length = this.readLength(additional)
        if (length > this.bytes.length - this.offset) throw new Error('cbor: unexpected end')
        const bytes = this.bytes.slice(this.offset, this.offset + length)
        this.offset += length
        return new TextDecoder().decode(bytes)
      }
      case 4: {
        const length = this.readLength(additional)
        const items: unknown[] = []
        for (let index = 0; index < length; index++) items.push(this.read())
        return items
      }
      case 5: {
        const length = this.readLength(additional)
        const record: Record<string, unknown> = {}
        for (let index = 0; index < length; index++) {
          const key = this.read()
          const value = this.read()
          if (typeof key !== 'string' && typeof key !== 'number') {
            throw new Error('cbor: unsupported map key')
          }
          record[String(key)] = value
        }
        return record
      }
      case 7:
        if (additional === 20) return false
        if (additional === 21) return true
        if (additional === 25 || additional === 26 || additional === 27) {
          return this.readFloat(additional)
        }
        throw new Error('cbor: unsupported simple value')
      default:
        throw new Error('cbor: unsupported major type')
    }
  }
}

function decodeCbor(bytes: Uint8Array): unknown {
  if (bytes.byteLength === 0) throw new Error('cbor: empty payload')
  const reader = new CborReader(bytes)
  const value = reader.read()
  if (reader.consumed() !== bytes.byteLength) throw new Error('cbor: trailing bytes')
  return value
}
