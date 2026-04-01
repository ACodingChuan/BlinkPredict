"use client";

import { usePrivy } from "@/lib/auth-client";
import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useUserStore } from "@/store/useUserStore";

export default function RequireAuth({ children }: { children: React.ReactNode }) {
  const { ready, user, authenticated } = usePrivy();
  const router = useRouter();
  const { setUser } = useUserStore();

  useEffect(() => {
    if (ready && !user) {
      router.push("/");
      return;
    }

    if (authenticated && user) {
      setUser({
        id: user.walletAddress,
        linkedAccounts: [{
          id: user.walletAddress,
          address: user.walletAddress,
          chainType: "solana",
          connectorType: "wallet-adapter",
          type: "wallet",
        }],
      });
      return;
    }

    setUser(null);
  }, [ready, authenticated, user, router, setUser]);

  if (!ready || !user) {
    return <div>Loading...</div>;
  }

  return <>{children}</>;
}
