import Foundation

/// Self-healing after a lost link: which part is missing (board, Wi-Fi, mirroring, fresh image), when to ask the user,
/// and when to give up. Pure: the caller polls it with the link state and the time.
struct Recovery {
    /// What the app sees of the link right now.
    struct Link: Equatable {
        var connected: Bool       // BLE link to the board is up
        var sta: Int?             // 1 while the board is on the Wi-Fi (nil = unknown)
        var ap: String?           // idle | connected | streaming
        var frameAt: Double       // when the newest decoded frame arrived
        var stalled = false       // the board itself gets no frames from AirPlay: ask the user at once
    }

    enum Part: Equatable { case board, network, mirroring, frames }

    struct Stage: Equatable {
        var part: Part
        var ask: Bool  // waited long enough: the user has to do something
    }

    enum Next: Equatable {
        case wait(Stage)
        case live
        case giveUp(String)
    }

    /// How long each part may come back by itself before the user is asked; then the whole limit.
    static let quiet: [Part: Double] = [.board: 30, .network: 30, .mirroring: 20, .frames: 20]
    static let limit = 300.0

    let started: Double
    let limit: Double
    private(set) var stage: Stage?
    private var partSince = 0.0

    init(at t: Double, limit: Double = Self.limit) { started = t; self.limit = limit }

    /// Live once the board is back, mirroring streams and a frame newer than the loss has been decoded.
    mutating func next(_ l: Link, at t: Double) -> Next {
        let part: Part? = !l.connected ? .board
            : l.sta == 0 ? .network
            : l.ap != "streaming" ? .mirroring
            : l.frameAt <= started ? .frames
            : nil
        guard let part else { stage = nil; return .live }
        if part != stage?.part { partSince = t }
        stage = Stage(part: part, ask: t - partSince >= (part == .frames && l.stalled ? 0 : Self.quiet[part]!))
        if t - started >= limit { return .giveUp(Self.reason(part)) }
        return .wait(stage!)
    }

    static func reason(_ part: Part) -> String {
        switch part {
        case .board: return String(localized: "the board did not come back in 5 minutes")
        case .network: return String(localized: "the board did not rejoin the Wi-Fi in 5 minutes")
        case .mirroring: return String(localized: "Screen Mirroring did not come back in 5 minutes")
        case .frames: return String(localized: "no fresh screen image arrived in 5 minutes")
        }
    }
}

extension Recovery.Stage {
    /// For the log: "recover: …".
    var logLine: String {
        switch (part, ask) {
        case (.board, false): return "BLE link lost; reconnecting"
        case (.board, true): return "board still gone; asked the user to check it"
        case (.network, false): return "board up, not on the Wi-Fi (sta 0); waiting for it to rejoin"
        case (.network, true): return "board still off the Wi-Fi; asked the user to check it"
        case (.mirroring, false): return "board and Wi-Fi up, no mirroring session; waiting for iOS to resume mirroring"
        case (.mirroring, true): return "mirroring still off; asked the user to start it"
        case (.frames, false): return "mirroring session active, no fresh decoded image; waiting for a frame"
        case (.frames, true): return "mirroring session active, still no fresh decoded image; asked the user to reconnect mirroring"
        }
    }

    /// Dynamic Island label: compact and full.
    var status: IslandStatus {
        switch (part, ask) {
        case (.board, _):
            return .waitingForBoard
        case (.network, false):
            return IslandStatus(short: String(localized: "Joining Wi-Fi…"), full: String(localized: "Waiting for the board to rejoin the Wi-Fi…"),
                                look: nil, minimal: String(localized: "Wi-Fi"))
        case (.network, true):
            return IslandStatus(short: String(localized: "Check Wi-Fi"), full: String(localized: "Check the board's Wi-Fi to continue"),
                                look: nil, minimal: String(localized: "Wi-Fi"))
        case (.mirroring, false):
            return IslandStatus(short: String(localized: "Mirroring…"), full: String(localized: "Waiting for mirroring…"),
                                look: nil, minimal: String(localized: "Mirror"))
        case (.mirroring, true):
            return IslandStatus(short: String(localized: "Start Mirroring"),
                                full: String(localized: "Tap Screen Mirroring → ESP32 Agent to continue"),
                                look: nil, minimal: String(localized: "Mirror"))
        case (.frames, false):
            return IslandStatus(short: String(localized: "Waiting frame…"),
                                full: String(localized: "Mirroring is connected. Waiting for a fresh screen image…"),
                                look: nil, minimal: String(localized: "Frame"))
        case (.frames, true):
            return IslandStatus(short: String(localized: "No new frame"),
                                full: String(localized: "Mirroring is connected, but no new screen image has arrived."),
                                look: nil, minimal: String(localized: "Frame"))
        }
    }

    /// One sentence for the Live Activity line, the in-app banner and the notification body.
    var line: String {
        switch (part, ask) {
        case (.board, _): return IslandStatus.waitingForBoard.full
        case (.network, false): return String(localized: "Waiting for the board to rejoin the Wi-Fi…")
        case (.network, true): return String(localized: "The board can't reach the Wi-Fi. Check the network or Settings › Board; the run goes on when it is back.")
        case (.mirroring, false): return String(localized: "Board is back. Waiting for Screen Mirroring to resume…")
        case (.mirroring, true): return String(localized: "Open Control Center, tap Screen Mirroring → ESP32 Agent; the run goes on from here.")
        case (.frames, false): return status.full
        case (.frames, true): return String(localized: "Stop and restart Screen Mirroring → ESP32 Agent; I’ll continue when a fresh image arrives.")
        }
    }
}
