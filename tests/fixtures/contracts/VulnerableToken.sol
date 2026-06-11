// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

// VulnerableToken is a deliberately broken ERC-20-ish token used as a
// fixture for uta's contract-audit pipeline. Real auditors (and the
// slither / aderyn adapters) should flag every one of the bugs below.
//
// Intentional issues:
//   1. `withdraw` is reentrant — external call before state update.
//   2. `mint` has no access control — any caller can inflate supply.
//   3. `transfer` does not check the recipient is non-zero.
//   4. `transfer` uses `tx.origin` for the sender check, which is unsafe
//      against phishing intermediary contracts.
//   5. `setAdmin` lets the admin renounce ownership to address(0) with
//      no recovery path.
//   6. The fallback `receive()` silently accepts ETH without crediting
//      the sender, so funds get stranded.
//
// DO NOT DEPLOY. Test fixture only.
contract VulnerableToken {
    string public name = "VulnerableToken";
    string public symbol = "VULN";
    uint8 public decimals = 18;

    uint256 public totalSupply;
    address public admin;

    mapping(address => uint256) public balanceOf;

    constructor() {
        admin = msg.sender;
    }

    // Issue 2: no `onlyAdmin` modifier — anyone can mint.
    function mint(address to, uint256 amount) external {
        balanceOf[to] += amount;
        totalSupply += amount;
    }

    // Issue 4: tx.origin for authorization is unsafe.
    function transfer(address to, uint256 amount) external returns (bool) {
        require(balanceOf[tx.origin] >= amount, "insufficient");
        balanceOf[tx.origin] -= amount;
        balanceOf[to] += amount; // Issue 3: no zero-address check on `to`
        return true;
    }

    // Issue 1: classic reentrancy — external call before state update.
    function withdraw(uint256 amount) external {
        require(balanceOf[msg.sender] >= amount, "insufficient");
        (bool ok, ) = msg.sender.call{value: amount}("");
        require(ok, "call failed");
        balanceOf[msg.sender] -= amount;
    }

    // Issue 5: no zero-address guard on the new admin.
    function setAdmin(address newAdmin) external {
        require(msg.sender == admin, "not admin");
        admin = newAdmin;
    }

    // Issue 6: ETH received but never credited.
    receive() external payable {}
}
