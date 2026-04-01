import { Buffer } from "buffer";
import { keccak_256 } from "@noble/hashes/sha3.js";
import BN from "bn.js";
import { PublicKey } from "@solana/web3.js";

export const ORDER_INTENT_VERSION = 1;

export interface OrderIntentFields {
  version: number;           // u8
  chainId: number;           // u16
  programId: Uint8Array;     // [u8; 32]
  market: Uint8Array;        // [u8; 32]
  user: Uint8Array;          // [u8; 32]
  side: number;              // u8: 0=Buy, 1=Sell
  outcome: number;           // u8: 0=Yes, 1=No
  orderType: number;         // u8: 0=Limit, 1=Market
  limitPrice: BN;            // u64 (price tick: 1..99, cents/share)
  totalAmount: BN;           // u64 (share lots or spend cents, both scaled by 100)
  nonce: BN;                 // u64
  expiryTs: BN;              // i64
}

// 固定长度 134 bytes
export function serializeOrderIntent(intent: OrderIntentFields): Uint8Array {
  const buffer = new Uint8Array(134);
  let offset = 0;

  buffer[offset] = intent.version;
  offset += 1;

  const chainIdBytes = new BN(intent.chainId).toArray("le", 2);
  buffer.set(chainIdBytes, offset);
  offset += 2;

  buffer.set(intent.programId, offset);
  offset += 32;

  buffer.set(intent.market, offset);
  offset += 32;

  buffer.set(intent.user, offset);
  offset += 32;

  buffer[offset] = intent.side;
  offset += 1;

  buffer[offset] = intent.outcome;
  offset += 1;

  buffer[offset] = intent.orderType;
  offset += 1;

  buffer.set(intent.limitPrice.toArray("le", 8), offset);
  offset += 8;

  buffer.set(intent.totalAmount.toArray("le", 8), offset);
  offset += 8;

  buffer.set(intent.nonce.toArray("le", 8), offset);
  offset += 8;

  buffer.set(intent.expiryTs.toArray("le", 8), offset);
  return buffer;
}

export function hashOrderIntent(intentBytes: Uint8Array): Uint8Array {
  return new Uint8Array(keccak_256(intentBytes));
}

export function buildOrderSignatureMessage(messageHash: Uint8Array): Uint8Array {
  const hexHash = Array.from(messageHash, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return new TextEncoder().encode(hexHash);
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
  chainId: number;
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
    chainId: params.chainId,
    programId: params.programId.toBytes(),
    market: params.market.toBytes(),
    user: params.user.toBytes(),
    side: params.side === "buy" ? 0 : 1,
    outcome: params.outcome === "yes" ? 0 : 1,
    orderType: params.orderType === "limit" ? 0 : 1,
    limitPrice: new BN(params.limitPrice),
    totalAmount: new BN(params.totalAmount),
    nonce,
    expiryTs: new BN(params.expiryTs),
  };

  const intentBytes = serializeOrderIntent(intent);
  const messageHash = hashOrderIntent(intentBytes);
  const signableMessage = new TextEncoder().encode(Buffer.from(messageHash).toString("hex"));
  return { intent, intentBytes, messageHash, signableMessage };
}

export function encodePriceToTick(price: number): number {
  return Math.round(price * 100);
}

export function encodeAmountToUnits(amount: number): number {
  return Math.round(amount * 100);
}

export const encodePriceToUnits = encodePriceToTick;
