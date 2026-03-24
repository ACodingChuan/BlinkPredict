import * as anchor from "@coral-xyz/anchor";
import { Program } from "@coral-xyz/anchor";
import { PredixProgram } from "../target/types/predix_program";
import { BN } from "bn.js";
import { PublicKey } from "@solana/web3.js";
import {
  approve,
  createAccount,
  createMint,
  getAssociatedTokenAddress,
  getOrCreateAssociatedTokenAccount,
  mintTo,
} from "@solana/spl-token";
import { expect } from "chai";

describe("BlinkPredict Contract", () => {
  const provider = anchor.AnchorProvider.env();
  anchor.setProvider(provider);
  const program = anchor.workspace.predixProgram as Program<PredixProgram>;

  const admin = anchor.web3.Keypair.generate();
  const creatorResolver = anchor.web3.Keypair.generate();
  const user = anchor.web3.Keypair.generate();
  const creatorMarketId = new BN(11);
  const pythMarketId = new BN(22);

  let collateralMint: PublicKey;
  let creatorMarketPda: PublicKey;
  let creatorVaultPda: PublicKey;
  let creatorYesMint: PublicKey;
  let creatorNoMint: PublicKey;

  const deriveAddresses = (marketId: BN) => {
    const [market] = anchor.web3.PublicKey.findProgramAddressSync(
      [Buffer.from("market"), marketId.toArrayLike(Buffer, "le", 8)],
      program.programId,
    );
    const [vault] = anchor.web3.PublicKey.findProgramAddressSync(
      [Buffer.from("collateral_vault"), marketId.toArrayLike(Buffer, "le", 8)],
      program.programId,
    );
    const [yesMint] = anchor.web3.PublicKey.findProgramAddressSync(
      [Buffer.from("yes_mint"), marketId.toArrayLike(Buffer, "le", 8)],
      program.programId,
    );
    const [noMint] = anchor.web3.PublicKey.findProgramAddressSync(
      [Buffer.from("no_mint"), marketId.toArrayLike(Buffer, "le", 8)],
      program.programId,
    );
    return { market, vault, yesMint, noMint };
  };

  before(async () => {
    for (const kp of [admin, creatorResolver, user]) {
      await provider.connection.requestAirdrop(kp.publicKey, 5 * anchor.web3.LAMPORTS_PER_SOL);
    }
    await new Promise((resolve) => setTimeout(resolve, 2000));

    collateralMint = await createMint(
      provider.connection,
      admin,
      admin.publicKey,
      null,
      6,
    );

    const creatorDerived = deriveAddresses(creatorMarketId);
    creatorMarketPda = creatorDerived.market;
    creatorVaultPda = creatorDerived.vault;
    creatorYesMint = creatorDerived.yesMint;
    creatorNoMint = creatorDerived.noMint;
  });

  it("initializes a creator resolved market", async () => {
    const metadataURL = "https://example.com/creator.json";
    const closeTime = new BN(Math.floor(Date.now() / 1000) + 60 * 60);
    const resolveAfter = new BN(closeTime.toNumber() + 60);
    const emptyFeedId = new Array(32).fill(0);

    await program.methods
      .initializeMarket(
        creatorMarketId,
        metadataURL,
        closeTime,
        resolveAfter,
        { creator: {} },
        emptyFeedId,
        { greaterThan: {} },
        new BN(0),
        0,
      )
      .accounts({
        market: creatorMarketPda,
        vault: creatorVaultPda,
        collateralMint,
        yesMint: creatorYesMint,
        noMint: creatorNoMint,
        admin: admin.publicKey,
      })
      .signers([admin])
      .rpc();

    const marketAccount = await program.account.market.fetch(creatorMarketPda);
    expect(marketAccount.resolutionMode).to.deep.equal({ creator: {} });
    expect(marketAccount.oracleFeedId).to.deep.equal(emptyFeedId);
  });

  it("splits, merges, resolves, and claims a creator market", async () => {
    const amount = new BN(2_000_000);
    const userCollateral = await createAccount(
      provider.connection,
      user,
      collateralMint,
      user.publicKey,
    );

    await mintTo(provider.connection, admin, collateralMint, userCollateral, admin.publicKey, 3_000_000);
    await approve(provider.connection, user, userCollateral, creatorMarketPda, user.publicKey, amount.toNumber());

    const userYesAta = await getAssociatedTokenAddress(creatorYesMint, user.publicKey);
    const userNoAta = await getAssociatedTokenAddress(creatorNoMint, user.publicKey);

    await program.methods
      .splitToken(creatorMarketId, amount)
      .accounts({
        market: creatorMarketPda,
        user: user.publicKey,
        userCollateral,
        collateralVault: creatorVaultPda,
        yesMint: creatorYesMint,
        noMint: creatorNoMint,
        yesAta: userYesAta,
        noAta: userNoAta,
      })
      .signers([user])
      .rpc();

    let marketAccount = await program.account.market.fetch(creatorMarketPda);
    expect(marketAccount.yesTotal.toNumber()).to.equal(amount.toNumber());
    expect(marketAccount.noTotal.toNumber()).to.equal(amount.toNumber());

    await program.methods
      .mergeTokens(creatorMarketId, new BN(1_000_000))
      .accounts({
        market: creatorMarketPda,
        user: user.publicKey,
        userCollateral,
        collateralVault: creatorVaultPda,
        yesAta: userYesAta,
        noAta: userNoAta,
        yesMint: creatorYesMint,
        noMint: creatorNoMint,
      })
      .signers([user])
      .rpc();

    marketAccount = await program.account.market.fetch(creatorMarketPda);
    expect(marketAccount.yesTotal.toNumber()).to.equal(1_000_000);
    expect(marketAccount.noTotal.toNumber()).to.equal(1_000_000);

    await program.methods
      .resolveByCreator(creatorMarketId, { yes: {} })
      .accounts({
        market: creatorMarketPda,
        authority: admin.publicKey,
      })
      .signers([admin])
      .rpc();

    await program.methods
      .claimReward(creatorMarketId)
      .accounts({
        market: creatorMarketPda,
        user: user.publicKey,
        userCollateral,
        collateralVault: creatorVaultPda,
        yesAta: userYesAta,
        noAta: userNoAta,
        yesMint: creatorYesMint,
        noMint: creatorNoMint,
      })
      .signers([user])
      .rpc();

    const claimedYesBalance = await provider.connection.getTokenAccountBalance(userYesAta);
    expect(claimedYesBalance.value.uiAmount).to.equal(0);
  });

  it("initializes a pyth market configuration", async () => {
    const { market, vault, yesMint, noMint } = deriveAddresses(pythMarketId);
    const closeTime = new BN(Math.floor(Date.now() / 1000) + 2 * 60 * 60);
    const resolveAfter = new BN(Math.floor(Date.now() / 1000) + 60 * 60);
    const feedId = Array.from(Buffer.from("0".repeat(64), "hex"));

    await program.methods
      .initializeMarket(
        pythMarketId,
        "https://example.com/pyth.json",
        closeTime,
        resolveAfter,
        { pyth: {} },
        feedId,
        { greaterThanOrEqual: {} },
        new BN(250_000_000),
        -8,
      )
      .accounts({
        market,
        vault,
        collateralMint,
        yesMint,
        noMint,
        admin: admin.publicKey,
      })
      .signers([admin])
      .rpc();

    const marketAccount = await program.account.market.fetch(market);
    expect(marketAccount.resolutionMode).to.deep.equal({ pyth: {} });
    expect(marketAccount.oracleFeedId).to.deep.equal(feedId);
    expect(marketAccount.oracleTargetPriceInt.toString()).to.equal("250000000");
    expect(marketAccount.oracleTargetExpo).to.equal(-8);
  });
});
