import { PublicKey } from "@solana/web3.js";
import BN from 'bn.js';

type TradeSide = "Yes" | "No";
export type MatchFill = {
  side: { yes: {} } | { no: {} };
  shares:  BN;
  price:  BN;
};

export function getRemainingAccounts(
  buyerCollateral: PublicKey[],
  sellerCollateral: PublicKey[],
  sellerAta: PublicKey[],
  buyerAta: PublicKey[],
  buyerOwner: PublicKey[],
  sellerOwner: PublicKey[]
) {
  const accounts = [];
    for (let i = 0; i < buyerCollateral.length; i++) {
        accounts.push({ pubkey: buyerCollateral[i], isWritable: true, isSigner: false });
        accounts.push({ pubkey: sellerCollateral[i], isWritable: true, isSigner: false });
        accounts.push({ pubkey: sellerAta[i], isWritable: true, isSigner: false });
        accounts.push({ pubkey: buyerAta[i], isWritable: true, isSigner: false });
        accounts.push({ pubkey: buyerOwner[i], isWritable: false, isSigner: false });
        accounts.push({ pubkey: sellerOwner[i], isWritable: false, isSigner: false });
    }
  return accounts;
}

function sideToEnum(side: 'Yes' | 'No') {
  return side === 'Yes' ? { yes: {} } as const : { no: {} } as const;
}

export function getMatchFills(
  shares: bigint[],
  price: bigint[],
  side: TradeSide[]
): MatchFill[] {
  const fills: MatchFill[] = [];
  for (let i = 0; i < shares.length; i++) {
    fills.push({
      side: sideToEnum(side[i]),
      shares: new BN(shares[i].toString()),
      price: new BN(price[i].toString()),
    });
  }
  return fills;
}
