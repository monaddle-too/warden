from pathlib import Path
from network_control import load_firewall
load_firewall(Path(__file__).with_name('firewall.nft'))
