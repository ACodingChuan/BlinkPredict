import { Buffer } from "buffer";
import { keccak_256 } from "@noble/hashes/sha3.js";
import BN from "bn.js";
import { PublicKey } from "@solana/web3.js";

export const ORDER_INTENT_VERSION = 1;
export const ORDER_INTENT_DOMAIN = "predix-order";
export const ORDER_SIGNATURE_PREFIX = "bp1:";
export const ORDER_INTENT_SIZE = 118;

export interface OrderIntentFields {
  version: number;
  programId: Uint8Array;     // [u8; 32]
  market: Uint8Array;        // [u8; 32]
  user: Uint8Array;          // [u8; 32]
  nonce: BN;                 // u64
  side: number;              // u8: 0=Buy, 1=Sell
  outcome: number;           // u8: 0=Yes, 1=No
  orderType: number;         // u8: 0=Limit, 1=Market
  limitPrice: number;        // u8 (price tick: 1..99)
  totalAmount: BN;           // u64 (share lots or spend cents, both scaled by 100)
  expiryTs: number;          // u32
}

function orderFlags(intent: Pick<OrderIntentFields, "side" | "outcome" | "orderType">): number {
  let flags = 0;
  if (intent.side === 1) flags |= 1 << 0;
  if (intent.outcome === 1) flags |= 1 << 1;
  if (intent.orderType === 1) flags |= 1 << 2;
  return flags;
}

function toU32LE(value: number): Uint8Array {
  const out = new Uint8Array(4);
  const view = new DataView(out.buffer);
  view.setUint32(0, value, true);
  return out;
}

function toBase64UrlNoPad(bytes: Uint8Array): string {
  return Buffer.from(bytes)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "");
}

// Fixed length 118 bytes, matching backend/contract OrderIntentV1.
export function serializeOrderIntent(intent: OrderIntentFields): Uint8Array {
  const buffer = new Uint8Array(ORDER_INTENT_SIZE);
  let offset = 0;

  buffer.set(intent.programId, offset);
  offset += 32;

  buffer.set(intent.market, offset);
  offset += 32;

  buffer.set(intent.user, offset);
  offset += 32;

  buffer.set(intent.nonce.toArray("le", 8), offset);
  offset += 8;

  buffer[offset] = orderFlags(intent);
  offset += 1;

  buffer[offset] = intent.limitPrice;
  offset += 1;

  buffer.set(intent.totalAmount.toArray("le", 8), offset);
  offset += 8;

  buffer.set(toU32LE(intent.expiryTs), offset);
  return buffer;
}

export function hashOrderIntent(intentBytes: Uint8Array): Uint8Array {
  const domainBytes = new TextEncoder().encode(ORDER_INTENT_DOMAIN);
  const payload = new Uint8Array(domainBytes.length + intentBytes.length);
  payload.set(domainBytes, 0);
  payload.set(intentBytes, domainBytes.length);
  return new Uint8Array(keccak_256(payload));
}

export function buildOrderSignatureMessage(messageHash: Uint8Array): Uint8Array {
  return new TextEncoder().encode(`${ORDER_SIGNATURE_PREFIX}${toBase64UrlNoPad(messageHash)}`);
}

// 42 位毫秒时间戳 + 22 位随机数
export function generateSecureNonce(): BN {
  const now = BigInt(Date.now());
  const randomBytes = new Uint8Array(3);
  crypto.getRandomValues(randomBytes);
  const randomPart = BigInt((randomBytes[0] << 16) | (randomBytes[1] << 8) | randomBytes[2]) & BigInt(0x3fffff);
  return new BN(((now << BigInt(22)) | randomPart).toString());
}

export interface RawBuildOrderIntentParams {
  version?: number;
  programId: PublicKey;
  market: PublicKey;
  user: PublicKey;
  side: "buy" | "sell";
  outcome: "yes" | "no";
  orderType: "limit" | "market";
  limitPrice: number;
  totalAmount: number;
  expiryTs: number;
  nonce?: BN;
}
export type BuildOrderIntentParams = RawBuildOrderIntentParams;

export function buildOrderIntent(params: BuildOrderIntentParams): {
  intent: OrderIntentFields;
  intentBytes: Uint8Array;
  messageHash: Uint8Array;
  signableMessage: Uint8Array;
} {
  const nonce = params.nonce || generateSecureNonce();
  const intent: OrderIntentFields = {
    version: params.version ?? ORDER_INTENT_VERSION,
    programId: params.programId.toBytes(),
    market: params.market.toBytes(),
    user: params.user.toBytes(),
    nonce,
    side: params.side === "buy" ? 0 : 1,
    outcome: params.outcome === "yes" ? 0 : 1,
    orderType: params.orderType === "limit" ? 0 : 1,
    limitPrice: params.limitPrice,
    totalAmount: new BN(params.totalAmount),
    expiryTs: params.expiryTs,
  };

  const intentBytes = serializeOrderIntent(intent);
  const messageHash = hashOrderIntent(intentBytes);
  const signableMessage = buildOrderSignatureMessage(messageHash);
  return { intent, intentBytes, messageHash, signableMessage };
}

export function encodePriceToTick(price: number): number {
  return Math.round(price * 100);
}

export function encodeAmountToUnits(amount: number): number {
  return Math.round(amount * 100);
}

export const encodePriceToUnits = encodePriceToTick;
