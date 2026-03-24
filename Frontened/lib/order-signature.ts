import { Buffer } from "buffer";
import { keccak_256 } from '@noble/hashes/sha3.js';
import BN from 'bn.js';
import { PublicKey } from '@solana/web3.js';

// 订单意图 Borsh Schema (严格定长 107 字节)
// 根据订单系统全局规范 V1 (orderDesign.md)

// 固定的 ProgramID (需要从配置获取，这里先用占位符)
// TODO: 从环境变量或配置文件获取真实的 ProgramID
const PROGRAM_ID_PLACEHOLDER = new Uint8Array(32).fill(0);

// 订单意图 Borsh 结构定义
export interface OrderIntentFields {
  programId: Uint8Array;     // [u8; 32] 防跨合约重放
  walletAddress: Uint8Array; // [u8; 32] 用户钱包
  marketId: BN;              // u64 市场ID
  originalAction: number;    // u8: 0=Buy, 1=Sell
  originalOutcome: number;   // u8: 0=Yes, 1=No
  originalPriceTick: number; // u8: 用户原始价格/滑点边界
  side: number;              // u8: 0=Buy, 1=Sell (已归一化为 Yes)
  orderType: number;         // u8: 0=Limit, 1=Market
  priceTick: number;         // u8: 1-99 (挂单价或市价滑点底线)
  qtyLots: BN;               // u64: 份额 (乘 100 后的整数)
  spendAmount: BN;           // u64: 金额 (乘 100 后的整数)
  expireTime: BN;            // i64: Unix 秒级时间戳
  nonce: BN;                 // u64: 时间戳+随机数防碰撞
}

// 序列化订单意图为 107 字节数组（手动构建，确保精确控制）
export function serializeOrderIntent(intent: OrderIntentFields): Uint8Array {
  const buffer = new Uint8Array(110);
  let offset = 0;

  // ProgramID [32]byte
  buffer.set(intent.programId, offset);
  offset += 32;

  // WalletAddress [32]byte
  buffer.set(intent.walletAddress, offset);
  offset += 32;

  // MarketID u64 (小端序)
  const marketIdBytes = intent.marketId.toArray('le', 8);
  buffer.set(marketIdBytes, offset);
  offset += 8;

  buffer[offset] = intent.originalAction;
  offset += 1;

  buffer[offset] = intent.originalOutcome;
  offset += 1;

  buffer[offset] = intent.originalPriceTick;
  offset += 1;

  // Side u8
  buffer[offset] = intent.side;
  offset += 1;

  // OrderType u8
  buffer[offset] = intent.orderType;
  offset += 1;

  // PriceTick u8
  buffer[offset] = intent.priceTick;
  offset += 1;

  // QtyLots u64 (小端序)
  const qtyLotsBytes = intent.qtyLots.toArray('le', 8);
  buffer.set(qtyLotsBytes, offset);
  offset += 8;

  // SpendAmount u64 (小端序)
  const spendAmountBytes = intent.spendAmount.toArray('le', 8);
  buffer.set(spendAmountBytes, offset);
  offset += 8;

  // ExpireTime i64 (小端序)
  const expireTimeBytes = intent.expireTime.toArray('le', 8);
  buffer.set(expireTimeBytes, offset);
  offset += 8;

  // Nonce u64 (小端序)
  const nonceBytes = intent.nonce.toArray('le', 8);
  buffer.set(nonceBytes, offset);

  return buffer;
}

// 计算订单意图的 Keccak256 哈希
export function hashOrderIntent(intentBytes: Uint8Array): Uint8Array {
  return new Uint8Array(keccak_256(intentBytes));
}

export function buildOrderSignatureMessage(messageHash: Uint8Array): Uint8Array {
  const hexHash = Array.from(messageHash, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return new TextEncoder().encode(hexHash);
}

// 构建防碰撞 Nonce (42位时间戳 + 22位随机数)
export function generateSecureNonce(): BN {
  const now = BigInt(Date.now());
  const randomBytes = new Uint8Array(3);
  crypto.getRandomValues(randomBytes);
  const randomPart = BigInt((randomBytes[0] << 16) | (randomBytes[1] << 8) | randomBytes[2]) & BigInt(0x3FFFFF);
  return new BN(((now << BigInt(22)) | randomPart).toString());
}

// 构建订单意图参数
export interface BuildOrderIntentParams {
  programId?: PublicKey;      // 可选，默认使用占位符
  walletAddress: PublicKey;   // 用户钱包地址
  marketId: string;           // 市场ID (字符串形式的 uint64)
  originalAction: "buy" | "sell";
  originalOutcome: "yes" | "no";
  originalPriceTick: number;
  side: "buy" | "sell";       // 已归一化为 Yes 的 side
  orderType: "limit" | "market";
  priceTick: number;          // 1-99
  qtyLots: number;            // 份额 (乘 100 后的整数)
  spendAmount: number;        // 金额 (乘 100 后的整数)
  expireTime: number;         // Unix 秒级时间戳 (0=GTC)
  nonce?: BN;                 // 可选，不传则自动生成
}

// 构建订单意图并序列化
export function buildOrderIntent(params: BuildOrderIntentParams): {
  intent: OrderIntentFields;
  intentBytes: Uint8Array;
  messageHash: Uint8Array;
  signableMessage: Uint8Array;
} {
  // 生成 nonce (如果不提供)
  const nonce = params.nonce || generateSecureNonce();

  // 构建 ProgramID
  let programIdBytes: Uint8Array;
  if (params.programId) {
    programIdBytes = params.programId.toBytes();
  } else {
    programIdBytes = PROGRAM_ID_PLACEHOLDER;
  }

  // 构建订单意图
  const intent: OrderIntentFields = {
    programId: programIdBytes,
    walletAddress: params.walletAddress.toBytes(),
    marketId: new BN(params.marketId),
    originalAction: params.originalAction === "buy" ? 0 : 1,
    originalOutcome: params.originalOutcome === "yes" ? 0 : 1,
    originalPriceTick: params.originalPriceTick,
    side: params.side === "buy" ? 0 : 1,
    orderType: params.orderType === "limit" ? 0 : 1,
    priceTick: params.priceTick,
    qtyLots: new BN(params.qtyLots),
    spendAmount: new BN(params.spendAmount),
    expireTime: new BN(params.expireTime),
    nonce,
  };

  // 序列化
  const intentBytes = serializeOrderIntent(intent);

  // 计算哈希
  const messageHash = hashOrderIntent(intentBytes);
  const signableMessage = new TextEncoder().encode(Buffer.from(messageHash).toString("hex"));

  return { intent, intentBytes, messageHash, signableMessage };
}

// 辅助函数：归一化 No 订单为 Yes 订单
export interface NormalizedOrderParams {
  action: "buy" | "sell";
  outcome: "yes" | "no";
  price: number;
  amount: number;
  type: "limit" | "market";
  orderBookAsks?: Array<{ price: string }>; // 可选，用于市价单滑点计算
  orderBookBids?: Array<{ price: string }>; // 可选，用于市价单滑点计算
}

export interface NormalizedOrderResult {
  originalAction: "buy" | "sell";
  originalOutcome: "yes" | "no";
  originalPriceTick: number;
  side: "buy" | "sell";       // 归一化后的 side (只操作 Yes)
  priceTick: number;          // 归一化后的 priceTick
  lotsOrAmount: number;       // 精度放大后的数值 (乘 100)
  isMarketBuy: boolean;       // 是否为市价买入 (用于判断字段映射)
}

// 归一化订单参数 (No -> Yes 转换)
export function normalizeOrderParams(params: NormalizedOrderParams): NormalizedOrderResult {
  // 1. 精度放大 (乘以 100 取整)
  const tick = Math.round(params.price * 100);
  const lotsOrAmount = Math.round(params.amount * 100);
  let originalPriceTick = tick;

  // 2. 标的归一化 (Buy No -> Sell Yes)
  let finalSide: "buy" | "sell" = params.action;
  let finalPriceTick = tick;

  if (params.outcome === "no") {
    finalSide = params.action === "buy" ? "sell" : "buy";
    finalPriceTick = 100 - tick;
  }

  const isMarketBuy = params.type === "market" && finalSide === "buy";
  if (params.type === "market") {
    originalPriceTick = params.action === "buy" ? 99 : 1;
    // Until the frontend consumes live best bid/ask directly, use permissive slippage bounds:
    // - market buy can cross the full ask book up to 99
    // - market sell can cross the full bid book down to 1
    finalPriceTick = finalSide === "buy" ? 99 : 1;
  }

  return {
    originalAction: params.action,
    originalOutcome: params.outcome,
    originalPriceTick,
    side: finalSide,
    priceTick: finalPriceTick,
    lotsOrAmount,
    isMarketBuy,
  };
}

// 旧版签名消息构建函数 (已废弃，保留用于向后兼容)
// @deprecated 使用 buildOrderIntent 替代
export type SignedOrderIntent = {
  walletAddress: string;
  marketId: string;
  side: "buy" | "sell";
  share: "yes" | "no";
  orderType: "limit" | "market";
  priceTick?: number;
  qtyLots: number;
  expireTime?: string;
  clientOrderId: string;
  signatureNonce: string;
  signedAt: string;
};

export const ORDER_INTENT_VERSION = "blinkpredict.order.v1";

// @deprecated 使用 buildOrderIntent 替代
export function buildOrderIntentMessage(intent: SignedOrderIntent): string {
  const normalizedOrderType = intent.orderType === "market" ? "market" : "limit";
  const normalizedSide = intent.side.toLowerCase();
  const normalizedShare = intent.share.toLowerCase();
  const normalizedPriceTick = normalizedOrderType === "limit" ? intent.priceTick ?? 0 : 0;
  const normalizedExpireTime = normalizedOrderType === "limit" ? (intent.expireTime || "") : "";

  return [
    ORDER_INTENT_VERSION,
    `wallet_address=${intent.walletAddress.trim()}`,
    `market_id=${intent.marketId.trim()}`,
    `side=${normalizedSide}`,
    `share=${normalizedShare}`,
    `order_type=${normalizedOrderType}`,
    `price_tick=${normalizedPriceTick}`,
    `qty_lots=${intent.qtyLots}`,
    `expire_time=${normalizedExpireTime.trim()}`,
    `client_order_id=${intent.clientOrderId.trim()}`,
    `signature_nonce=${intent.signatureNonce.trim()}`,
    `signed_at=${intent.signedAt.trim()}`,
  ].join("\n");
}
