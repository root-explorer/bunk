# Sharing a Windows machine (host side)

This is the one-page guide for the person **lending** their machine (the
host). They need Docker and the bunk agent; everything else happens on the
other side. No firewall changes, no port forwarding, no VPN — the agent
dials **out** to the hub, so it works behind any router/CGNAT.

## What gets installed

- **Docker Desktop** (the Docker engine runs in WSL2)
- **bunk** — one static binary, running inside the Ubuntu (WSL) terminal

## Steps (≈10 minutes)

### 1. Install Docker Desktop

1. Download from <https://www.docker.com/products/docker-desktop/> and run
   the installer. Keep the default **"Use WSL 2 based engine"** option.
2. Reboot if Windows asks you to, then start Docker Desktop once and wait
   until the whale icon is steady.

### 2. Open the Ubuntu terminal

Docker Desktop (via WSL) installs an **Ubuntu** app on your machine.

- Open **Ubuntu** from the Start menu.
- If it isn't there, open **PowerShell** and run: `wsl --install -d Ubuntu`
  (then open Ubuntu).

### 3. Run the installer (one line)

In the Ubuntu terminal, paste the command your sharing partner sent you. It
looks like this:

```bash
curl -fsSL https://raw.githubusercontent.com/root-explorer/bunk/main/scripts/install-agent.sh \
  | sudo bash -s -- ws://HUB-ADDRESS:8080/ws HUB-TOKEN brother-rig
```

(`brother-rig` is the name your machine gets — change it to anything.)

### 4. Send the pairing code

The installer ends by printing a pairing code like `K4F-9TX`. Send that code
to your sharing partner (it's one-time). They run `bunk pair K4F-9TX` and
that's it — your machine is linked.

## What they can do

- Run containers on your machine with normal `docker` commands — with safe
  default limits (6 CPU / 12 GB / 256 processes) unless they pass their own
  flags. Tune these in `/root/.bunk/config.yaml`.
- Access published ports from their own machine via auto-forwarding.

## GPU

- **Nvidia:** works out of the box if your Windows NVIDIA driver is recent.
  Inside the Ubuntu terminal, `nvidia-smi` should print your GPU. Bunk
  detects it automatically and passes it through (`--gpus all`).
- **AMD / Intel / no GPU:** everything still works, CPU-only.

## Cutting access off

The sharing partner can revoke your machine at any time (`bunk revoke`).
You can also stop lending entirely:

```bash
sudo systemctl stop bunk-agent && sudo systemctl disable bunk-agent
```

## Troubleshooting

- **`nvidia-smi` not found / GPU not detected:** update your NVIDIA driver
  from nvidia.com, then restart Docker Desktop.
- **Containers work but slowly:** check `/root/.bunk/config.yaml` — the
  default limits cap containers; raise them if you want to lend more.
- **Agent not running after reboot:** `sudo systemctl status bunk-agent`.
  If WSL2 doesn't start it, run `sudo systemctl start bunk-agent`.
