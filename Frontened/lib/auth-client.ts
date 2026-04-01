export { useAuthSession as usePrivy } from "@/components/auth-provider";

import { useAuthSession } from "@/components/auth-provider";

export function useIdentityToken() {
  const { identityToken } = useAuthSession();
  return { identityToken };
}
