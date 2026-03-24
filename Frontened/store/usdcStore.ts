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
const TOKEN_PROGRAM_ID = new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
const TOKEN_2022_PROGRAM_ID = new PublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
const ASSOCIATED_TOKEN_PROGRAM_ID = new PublicKey("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
let latestSyncJob = 0;

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
      const connection = new Connection(process.env.NEXT_PUBLIC_RPC_URL || "https://api.devnet.solana.com");
      const walletPublicKey = new PublicKey(walletAddress);
      const vusdcMint = process.env.NEXT_PUBLIC_VUSDC_MINT || "";
      if (!vusdcMint) {
        set({ balance: "0.00" });
        return;
      }
      const mintPubkey = new PublicKey(vusdcMint);

      let decimals = get().decimals;
      if (!Number.isFinite(decimals) || decimals < 0) {
        decimals = DEFAULT_DECIMALS;
      }

      if (!get().decimalsLoaded) {
        try {
          const mintInfo = await connection.getParsedAccountInfo(mintPubkey);
          const parsed = mintInfo.value?.data;
          if (parsed && typeof parsed === "object" && "parsed" in parsed) {
            const parsedData = parsed as { parsed?: { info?: { decimals?: number } } };
            const parsedDecimals = parsedData.parsed?.info?.decimals;
            if (typeof parsedDecimals === "number") {
              decimals = parsedDecimals;
            }
          }
          set({ decimals, decimalsLoaded: true });
        } catch {
          // Keep fallback decimals; retry next time.
        }
      }

      const mintAccountInfo = await connection.getAccountInfo(mintPubkey);
      const primaryTokenProgram = mintAccountInfo?.owner?.equals(TOKEN_2022_PROGRAM_ID)
        ? TOKEN_2022_PROGRAM_ID
        : TOKEN_PROGRAM_ID;
      const secondaryTokenProgram = primaryTokenProgram.equals(TOKEN_2022_PROGRAM_ID)
        ? TOKEN_PROGRAM_ID
        : TOKEN_2022_PROGRAM_ID;

      const candidateAtas = [
        deriveAta(walletPublicKey, mintPubkey, primaryTokenProgram),
        deriveAta(walletPublicKey, mintPubkey, secondaryTokenProgram),
      ];

      let amount = BigInt(0);
      for (const ata of candidateAtas) {
        const info = await connection.getAccountInfo(ata);
        if (!info) continue;
        if (!info.owner.equals(TOKEN_PROGRAM_ID) && !info.owner.equals(TOKEN_2022_PROGRAM_ID)) continue;

        const tokenBalance = await connection.getTokenAccountBalance(ata);
        amount = BigInt(tokenBalance.value.amount || "0");
        break;
      }

      if (amount === BigInt(0)) {
        const parsedByOwner = await connection.getParsedTokenAccountsByOwner(walletPublicKey, { mint: mintPubkey });
        for (const item of parsedByOwner.value) {
          const parsed = item.account.data.parsed as { info?: { tokenAmount?: { amount?: string } } };
          const raw = parsed?.info?.tokenAmount?.amount;
          if (raw) amount += BigInt(raw);
        }
      }

      set({ balance: formatBaseUnits(amount, decimals) });
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
      const delayMs = schedule[0] ?? 0;
      if (jobId !== latestSyncJob) return;
      if (delayMs > 0) {
        await sleep(delayMs);
        if (jobId !== latestSyncJob) return;
      }
      await get().fetchBalance(walletAddress);
    } finally {
      if (jobId === latestSyncJob) {
        set({ isRefreshing: false });
      }
    }
  },
}));
