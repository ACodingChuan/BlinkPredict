export function generateClientSnowflake(): string {
  const timestamp = BigInt(Date.now());
  const randomBytes = new Uint8Array(3);
  crypto.getRandomValues(randomBytes);
  const random = BigInt((randomBytes[0] << 16) | (randomBytes[1] << 8) | randomBytes[2]) & BigInt(0x3fffff);
  return ((timestamp << BigInt(22)) | random).toString();
}
