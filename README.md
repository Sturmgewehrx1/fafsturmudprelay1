# FAF Sturm Relay Adapter Configuration
#
# Installation (Client/Player):
# 1.) Locate the FAF Client\natives\ folder where the faf-ice-adapter.jar file is located.
# 2.) Rename faf-ice-adapter.jar in the natives folder (example: old_faf-ice-adapter.jar).
# 3.) Download the 3 files (build/client/) from the Github repo at the top right via "Download Raw File":
#       faf-ice-adapter.jar
#       faf-sturm-relay-adapter.exe
#       faf-sturm-relay.conf
# 4.) Copy the 3 files to the FAF Client\natives\ folder.
# 5.) Done.
#
# If this file "faf-sturm-relay.conf" is missing, all settings use their defaults.
# Lines starting with # or ; are comments.

# Use official FAF ICE connections when hosting a lobby.
#   false (default) = Use dedicated Sturm relay server (reduces 66 complex
#                     individual connections for 12 players to 12 stable
#                     dedicated server connections)
#                     – Reduces upload usage for players
#                     – DDoS protected (private IPs are not exposed to
#                       other players)
#                     – Better stability
#                     – Only compatible with other faf-sturm-relay-adapter
#                       users (players joining must also use
#                       faf-sturm-relay-adapter)
#   true            = Use official FAF ICE connections (P2P via STUN/TURN)
#                     Compatible with the old faf-ice-adapter
#
# This setting only affects the lobby CREATOR (host).
# Joining players automatically detect the host's mode.
# Note: Does not need to be changed for matchmaking; will be detected automatically.
Use_official_FAF_connections_as_lobby_creator_host = false
