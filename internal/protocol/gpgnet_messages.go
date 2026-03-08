package protocol

// GPGNet command names (adapter ↔ game)
const (
	CmdCreateLobby        = "CreateLobby"
	CmdHostGame           = "HostGame"
	CmdJoinGame           = "JoinGame"
	CmdConnectToPeer      = "ConnectToPeer"
	CmdDisconnectFromPeer = "DisconnectFromPeer"
	CmdGameState          = "GameState"
	CmdGameEnded          = "GameEnded"
	CmdGameFull           = "GameFull"
	CmdGameOption         = "GameOption"
	CmdChat               = "Chat"
	CmdJsonStats          = "JsonStats"
	CmdGameResult         = "GameResult"
)

// Game states as reported by ForgedAlliance.exe
const (
	GameStateNone      = "None"
	GameStateIdle      = "Idle"
	GameStateLobby     = "Lobby"
	GameStateLaunching = "Launching"
	GameStateEnded     = "Ended"
)

// LobbyInitMode values for CreateLobby command
const (
	LobbyInitModeNormal = 0 // normal lobby screen
	LobbyInitModeAuto   = 1 // skip lobby (matchmaker)
)
