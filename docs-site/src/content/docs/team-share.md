---
title: Team Share
description: What joining a team means, what your teammates can and cannot see, how a computer is shared with the team, and every control you have.
meta:
  audience: Anyone creating, joining, or sharing a computer with a team
  needs: A Waired account. Read this before creating or joining a team
  time: 8 minutes
---

This page is the full disclosure behind the consent message you see when you
create or join a team. Each section states what happens, and why.

## What Team Share is

A team is a small group of Waired accounts, up to 10, whose members lend
their computers to each other. When you are in a team, your requests may run
on a teammate's computer, and a teammate's requests may run on yours, if the
owner of that computer shares it with the team.

Team Share is different from [Public Share](/public-share/) in three ways:

- **You know each other.** Teammates see each other by name, not by
  nickname.
- **There is no requirement to share.** You can use your teammates'
  computers without sharing one of yours.
- **Nothing expires.** Access lasts as long as you are in the team and the
  computer stays shared with it.

## What your teammates can and cannot see

### The owner could see what you send

The owner of a teammate's computer could read what you send to it. Your
request is processed in plain form in that computer's memory, and its owner
fully controls that computer. The official app does not write your prompts
or replies to logs or disk, but the Waired client is open source and can be
modified, so that is our policy and the app's default behavior, not a
technical guarantee. Knowing someone does not change this, so it is
disclosed here the same way it is for Public Share.

### Your name, your computers, and sometimes your IP address

Teammates see your name as it appears in your Waired account, the names of
the computers you share with the team, and which models they run. When your
computer and a teammate's connect directly, each side can also see the
other's public IP address. When traffic goes through a relay, the other side
sees the relay's address instead. Which one happens is automatic, so treat
your IP address as possibly visible. Relayed traffic stays end-to-end
encrypted, and the relay cannot read it.

People outside your team see none of this. Your team's name and members are
not visible to anyone who is not in it.

### What Waired itself records

Waired records how much each shared computer is used: request counts, token
counts, duration, and which model. It never records what was asked or
answered, and prompts and replies never touch Waired's servers. The team
page shows usage per computer, not per member. See
[Privacy: what leaves your computer](/concepts/privacy/).

## Create or join a team

Teams are managed in the web console's **Team** tab. You can be in one team
at a time.

- **Create a team.** Give it a name. You become its owner.
- **Join a team.** Open the invite link a teammate sent you, sign in if
  asked, and accept. An invite link works for 7 days, can be used by more
  than one person, and can be cancelled by the team's admins at any time.

Creating and joining both show a consent message first. Your account needs a
name before you can do either, because your teammates see you by it. Set it
on the **Account** page.

## Share a computer with the team

Sharing is set per computer, on the **Sharing** card of the computer's page
in the web console. While you are in a team, the card has a third switch,
**Share with team**, next to **Your other computers** and **People outside
your account**. You can turn on any combination.

- **Only the owner of a computer can share it.** Nobody else in the team can
  turn it on for you.
- **Stopping is immediate.** Turning **Share with team** off cuts off any
  teammate's request running on that computer at that moment.
- **It is linked with Your other computers.** Turning **Share with team** on
  also turns **Your other computers** on, and turning **Your other
  computers** off also turns team sharing off, because a computer you are
  not lending to your own machines is not lent to anyone else.
- **The computer's own switch still wins.** `waired share off`, or **Stop
  sharing this computer** in the Waired app, stops serving your team along
  with everyone else.

A team's admins can take a shared computer out of the team's pool. That
stops new team requests from reaching it and lets running ones finish. It
never affects the owner's own use of the computer, and the owner's switch
stays theirs: while an admin has the computer out of the pool, the card says
so, and it rejoins the pool only when both the admin and the owner have it
on.

`waired share status` shows whether this computer is shared with the team:

```text
Sharing this computer: on
Your other computers: on
Your team: on
People outside your account: off
Who this computer is shared with is set in the Waired console.
```

## How teammates' computers are used

A teammate's shared computer is treated like one of your own. When Waired
picks a computer for a request, your computers and your teammates' are
considered together, using the same settings, and both come before public
nodes. The owner's settings for that computer still apply, for example
whether it serves main conversations or subagents.

If you would rather your requests never run on a teammate's computer, turn
that off for yourself in the **Team** tab. Your own shared computers keep
serving the team.

## Roles and leaving

A team has one owner, and any number of admins and members.

| Action | Owner | Admin | Member |
|---|---|---|---|
| Create and cancel invite links | Yes | Yes | No |
| Remove a member | Yes | Yes | No |
| Remove or demote an admin | Yes | No | No |
| Make a member an admin | Yes | Yes | No |
| Take a computer out of the team's pool | Yes | Yes | No |
| Rename or delete the team | Yes | No | No |
| Leave the team | Transfer ownership or delete the team first | Yes | Yes |

When you leave or are removed, your computers stop serving the team, your
requests stop reaching teammates' computers, and your computers' team
sharing is switched off. If you join a team again later, share them again
explicitly.

The owner cannot delete their Waired account while they own a team. Transfer
ownership to another member, or delete the team, first.

## Limits

- One team per account.
- Up to 10 members per team.
- Invite links last 7 days.
