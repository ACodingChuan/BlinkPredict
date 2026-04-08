import { create } from "zustand";
import { Connection, PublicKey } from "@solana/web3.js";

interface USDCState {
  balance: string;
  loading: boolean;
  isRefreshing: boolean;
  decimals: number;
  decimalsLoaded: boolean;
  fetchBalance: (walletAddress: string) => Promise<void>;
  syncBalance: (walletAddress: string, delaysMs?: number[]) => Promise<void>;
}

const DEFAULT_DECIMALS = 6;
const DEFAULT_SYNC_DELAYS_MS = [0];
const RPC_COMMITMENT = "confirmed";
const TOKEN_PROGRAM_ID = new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
const TOKEN_2022_PROGRAM_ID = new PublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
const ASSOCIATED_TOKEN_PROGRAM_ID = new PublicKey("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
let latestSyncJob = 0;
let sharedConnection: Connection | null = null;
let mintMetadataPromise: Promise<MintMetadata> | null = null;
let mintMetadataCache: MintMetadata | null = null;

interface MintMetadata {
  mint: string;
  decimals: number;
  tokenProgramId: PublicKey;
}

const sleep = (ms: number) =>
  new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });

const deriveAta = (owner: PublicKey, mint: PublicKey, tokenProgramId: PublicKey) =>
  PublicKey.findProgramAddressSync(
    [owner.toBuffer(), tokenProgramId.toBuffer(), mint.toBuffer()],
    ASSOCIATED_TOKEN_PROGRAM_ID,
  )[0];

const formatBaseUnits = (amount: bigint, decimals: number) => {
  if (decimals <= 0) return amount.toString();
  const zero = BigInt(0);
  const sign = amount < zero ? "-" : "";
  const absolute = amount < zero ? -amount : amount;
  const base = BigInt(10) ** BigInt(decimals);
  const integer = absolute / base;
  const fraction = absolute % base;
  const fractionStr = fraction
    .toString()
    .padStart(decimals, "0")
    .slice(0, 2)
    .padEnd(2, "0");
  return `${sign}${integer.toString()}.${fractionStr}`;
};

const getConnection = () => {
  if (sharedConnection) {
    return sharedConnection;
  }
  sharedConnection = new Connection(process.env.NEXT_PUBLIC_RPC_URL || "https://api.devnet.solana.com", RPC_COMMITMENT);
  return sharedConnection;
};

const readTokenAmount = (data: Uint8Array) => {
  if (!data || data.length < 72) {
    return BigInt(0);
  }
  let amount = BigInt(0);
  for (let index = 0; index < 8; index += 1) {
    amount |= BigInt(data[64 + index] ?? 0) << (BigInt(index) * BigInt(8));
  }
  return amount;
};

const loadMintMetadata = async (connection: Connection, mintPubkey: PublicKey) => {
  if (mintMetadataCache && mintMetadataCache.mint === mintPubkey.toBase58()) {
    return mintMetadataCache;
  }
  if (mintMetadataPromise) {
    return mintMetadataPromise;
  }
  mintMetadataPromise = (async () => {
    const response = await connection.getParsedAccountInfo(mintPubkey, RPC_COMMITMENT);
    const account = response.value;
    const owner = account?.owner;
    const tokenProgramId = owner?.equals(TOKEN_2022_PROGRAM_ID) ? TOKEN_2022_PROGRAM_ID : TOKEN_PROGRAM_ID;
    let decimals = DEFAULT_DECIMALS;
    const parsed = account?.data;
    if (parsed && typeof parsed === "object" && "parsed" in parsed) {
      const info = (parsed as { parsed?: { info?: { decimals?: number } } }).parsed?.info;
      if (typeof info?.decimals === "number") {
        decimals = info.decimals;
      }
    }
    mintMetadataCache = {
      mint: mintPubkey.toBase58(),
      decimals,
      tokenProgramId,
    };
    return mintMetadataCache;
  })();
  try {
    return await mintMetadataPromise;
  } finally {
    mintMetadataPromise = null;
  }
};

export const useUSDCStore = create<USDCState>((set, get) => ({
  balance: "0.00",
  loading: false,
  isRefreshing: false,
  decimals: DEFAULT_DECIMALS,
  decimalsLoaded: false,

  fetchBalance: async (walletAddress: string) => {
    if (!walletAddress) return;
    set({ loading: true });

    try {
      const connection = getConnection();
      const walletPublicKey = new PublicKey(walletAddress);
      const vusdcMint = process.env.NEXT_PUBLIC_VUSDC_MINT || "";
      if (!vusdcMint) {
        set({ balance: "0.00" });
        return;
      }
      const mintPubkey = new PublicKey(vusdcMint);
      const mintMetadata = await loadMintMetadata(connection, mintPubkey);
      const primaryTokenProgram = mintMetadata.tokenProgramId;
      const secondaryTokenProgram = primaryTokenProgram.equals(TOKEN_2022_PROGRAM_ID) ? TOKEN_PROGRAM_ID : TOKEN_2022_PROGRAM_ID;
      set({ decimals: mintMetadata.decimals, decimalsLoaded: true });

      const candidateAtas = [
        deriveAta(walletPublicKey, mintPubkey, primaryTokenProgram),
        deriveAta(walletPublicKey, mintPubkey, secondaryTokenProgram),
      ];

      let amount = BigInt(0);
      const candidateInfos = await connection.getMultipleAccountsInfo(candidateAtas, RPC_COMMITMENT);
      for (const info of candidateInfos) {
        if (!info) continue;
        if (!info.owner.equals(TOKEN_PROGRAM_ID) && !info.owner.equals(TOKEN_2022_PROGRAM_ID)) continue;
        amount += readTokenAmount(info.data);
      }

      if (amount === BigInt(0)) {
        const parsedByOwner = await connection.getParsedTokenAccountsByOwner(walletPublicKey, { mint: mintPubkey }, RPC_COMMITMENT);
        for (const item of parsedByOwner.value) {
          const parsed = item.account.data.parsed as { info?: { tokenAmount?: { amount?: string } } };
          const raw = parsed?.info?.tokenAmount?.amount;
          if (raw) amount += BigInt(raw);
        }
      }

      set({ balance: formatBaseUnits(amount, mintMetadata.decimals) });
    } catch (error) {
      console.error("Error fetching vUSDC balance:", error);
      set({ balance: "0.00" });
    } finally {
      set({ loading: false });
    }
  },

  syncBalance: async (walletAddress: string, delaysMs?: number[]) => {
    if (!walletAddress) return;

    const schedule = Array.isArray(delaysMs) && delaysMs.length > 0 ? delaysMs : DEFAULT_SYNC_DELAYS_MS;
    const jobId = ++latestSyncJob;
    set({ isRefreshing: true });

    try {
      for (const delayMs of schedule) {
        if (jobId !== latestSyncJob) return;
        if (delayMs > 0) {
          await sleep(delayMs);
          if (jobId !== latestSyncJob) return;
        }
        await get().fetchBalance(walletAddress);
        if (jobId !== latestSyncJob) return;
      }
    } finally {
      if (jobId === latestSyncJob) {
        set({ isRefreshing: false });
      }
    }
  },
}));
