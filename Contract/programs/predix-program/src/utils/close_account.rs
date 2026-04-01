use anchor_lang::prelude::*;
use anchor_lang::system_program;

use crate::error::ErrorCode;

pub fn close_program_account<'info>(
    account: &AccountInfo<'info>,
    receiver: &AccountInfo<'info>,
) -> Result<()> {
    require_keys_eq!(*account.owner, crate::ID, ErrorCode::InvalidAccountOwner);

    let lamports = account.lamports();
    let receiver_lamports = receiver.lamports();
    **receiver.lamports.borrow_mut() = receiver_lamports
        .checked_add(lamports)
        .ok_or(ErrorCode::MathOverflow)?;
    **account.lamports.borrow_mut() = 0;

    account.assign(&system_program::ID);
    account.resize(0)?;
    Ok(())
}
