"use client";

import { PrivyProvider } from "@privy-io/react-auth";
import { ThemeProvider } from "next-themes";
import { createSolanaRpc, createSolanaRpcSubscriptions } from "@solana/kit";
import { toSolanaWalletConnectors } from "@privy-io/react-auth/solana";

export default function Providers({ children }: { children: React.ReactNode }) {
  const appId = process.env.NEXT_PUBLIC_PRIVY_APP_ID;
  const clientId = process.env.NEXT_PUBLIC_PRIVY_CLIENT_ID;

  if (!appId || !clientId) {
    return (
      <ThemeProvider attribute="class" defaultTheme="system" enableSystem>
        {children}
      </ThemeProvider>
    );
  }

  const solanaConnectors = toSolanaWalletConnectors({ shouldAutoConnect: true });

  return (
    <PrivyProvider
      appId={appId}
      clientId={clientId}
      config={{
        embeddedWallets: {
          solana: { createOnLogin: "users-without-wallets" },
        },
        solana: {
          rpcs: {
            "solana:devnet": {
              rpc: createSolanaRpc(process.env.NEXT_PUBLIC_RPC_URL || "https://api.devnet.solana.com"),
              rpcSubscriptions: createSolanaRpcSubscriptions(process.env.NEXT_PUBLIC_WS_RPC_URL || "wss://api.devnet.solana.com"),
            },
          },
        },
        appearance: {
          theme: "dark",
          walletChainType: "solana-only",
        },
        externalWallets: {
          solana: { connectors: solanaConnectors },
        },
        loginMethods: ["wallet", "google"],
      }}
    >
      <ThemeProvider attribute="class" defaultTheme="system" enableSystem>
        {children}
      </ThemeProvider>
    </PrivyProvider>
  );
}
