import SwiftUI
import UIKit

/// 0 · Welcome: the character wakes up and says hello; the disclaimer must be accepted to go on.
struct WelcomePage: View {
    @Binding var accepted: Bool
    @State private var awake = false

    var body: some View {
        VStack(spacing: Space.xl) {
            PageHeader(mood: awake ? .idle : .sleeping, title: "Hi, I'm ESP32 Agent",
                       message: "Tell me a task and I'll do it on this iPhone: I see the screen through Screen Mirroring and tap with a USB mouse.")
            Panel {
                DisclosureGroup {
                    Text(Disclaimer.text).font(.footnote).foregroundStyle(.secondary).padding(.top, Space.s)
                } label: {
                    Label("Understand the risks", systemImage: "exclamationmark.shield").font(.body.weight(.semibold))
                }
                Divider().padding(.vertical, Space.m)
                Toggle("I understand and accept", isOn: $accepted)
            }
        }
        .task {
            try? await Task.sleep(nanoseconds: 700_000_000)
            awake = true
        }
    }
}

enum Disclaimer {
    static var text: LocalizedStringKey {
        "ESP32 Agent is experimental software that controls this iPhone on its own. It can tap the wrong thing, send messages, change settings, buy things or delete data. Watch it while it runs and stop it if anything looks wrong. Screenshots of your screen are sent to the model endpoint you choose. You use it at your own risk; the authors accept no liability for any loss or damage."
    }
}

/// 1 · Connect the board: Bluetooth permission, scan, pair (automatically when only one board is found).
struct ConnectPage: View {
    @ObservedObject var board: BoardLink

    var body: some View {
        VStack(spacing: Space.xl) {
            PageHeader(mood: board.connected ? .success : board.scanning ? .listening : .sleeping,
                       title: "Plug Me In",
                       message: "Connect the board to the USB-C port. I'll find it over Bluetooth; tap Pair when iOS asks.")
            if !board.connected { MiniPhoneDemo(kind: .plugIn) }
            Panel {
                if board.networkConfigured { boards } else {
                    switch board.state {
                    case .unauthorized:
                        Label("Bluetooth is not allowed for ESP32 Agent.", systemImage: "exclamationmark.triangle.fill")
                            .foregroundStyle(Palette.warning)
                        Button("Open Settings") { UIApplication.shared.open(URL(string: UIApplication.openSettingsURLString)!) }
                            .padding(.top, Space.s)
                    case .poweredOff:
                        Label("Turn on Bluetooth in Control Center.", systemImage: "antenna.radiowaves.left.and.right.slash")
                    default:
                        boards
                    }
                }
            }
            if !board.connected { FirmwareLinks() }
        }
        .onAppear { board.scan(); autoPair() }
        .onChange(of: board.state) { _, s in if s == .poweredOn { board.scan() } }
        .onChange(of: board.found) { autoPair() }
    }

    /// Only one board nearby: pair it without asking.
    private func autoPair() {
        if board.found.count == 1, board.pairedID == nil, let f = board.found.first { board.pair(f.id) }
    }

    /// Not connected → Found → Pairing → Connected. A paired board reconnects by itself when it is plugged in.
    @ViewBuilder private var boards: some View {
        if board.connected {
            CheckRow(title: "Connected to \(board.name.isEmpty ? "the board" : board.name)", done: true)
        } else if board.connecting {
            CheckRow(title: "Connecting to the board…", done: false, active: true, waiting: true)
        } else if board.linking {
            CheckRow(title: "Pairing… tap Pair when iOS asks", done: false, active: true, waiting: true)
        } else if board.pairedID == nil, board.found.count == 1, let f = board.found.first {
            CheckRow(title: "Found \(f.name)", done: false, active: true, waiting: true)
        } else if !board.found.isEmpty {
            ForEach(board.found) { f in
                Button { board.pair(f.id) } label: {
                    HStack {
                        Text("Pair \(f.name)").lineLimit(1)
                        Spacer()
                        Image(systemName: "chevron.right").foregroundStyle(.tertiary)
                    }
                    .padding(.vertical, Space.s)
                }
            }
        } else {
            CheckRow(title: "Searching for the board over Bluetooth…", done: false, active: true, waiting: board.scanning)
        }
        if board.pairingLost {
            // Short: a long warning pushed the button off small screens.
            Text("Pairing failed. Replug the board, then forget ESP32 Agent in Settings › Bluetooth.")
                .font(.footnote).foregroundStyle(Palette.warning).lineLimit(2).minimumScaleFactor(0.85)
                .padding(.top, Space.s)
        } else if !board.connected || (board.signalStrength ?? 0) < -80 {
            Text((board.signalStrength ?? 0) < -80
                 ? "Weak signal? Try placing the board right against the side of your phone, or attach its antenna."
                 : "No antenna? Try placing the board right against the side of your phone.")
                .font(.footnote).foregroundStyle(.secondary).padding(.top, Space.s)
        }
        if !board.connected && !board.pairingLost {
            if !board.lastError.isEmpty {
                Text(verbatim: board.lastError).font(.footnote).foregroundStyle(Palette.warning).lineLimit(2)
            }
        }
    }
}

/// A board fresh from the shop has no firmware and never shows up: point to the browser installer and the source.
struct FirmwareLinks: View {
    static let install = URL(string: "https://handsy.tech/install")!

    /// One line, so both links stay above the bottom button on every iPhone.
    var body: some View {
        HStack(spacing: Space.m) {
            Text("New board?").foregroundStyle(.secondary)
            Link(destination: Self.install) { Label { Text(verbatim: "handsy.tech/install") } icon: { Image(systemName: "desktopcomputer") } }
            Link(destination: AboutView.repo) {
                Label { Text(verbatim: "GitHub") } icon: {
                    Image("GitHubMark").renderingMode(.template).resizable().scaledToFit().frame(width: 14, height: 14)
                }
            }
        }
        .font(.footnote.weight(.semibold))
        .labelStyle(TightLabel())
        .lineLimit(1)
        .minimumScaleFactor(0.8)
        .tint(.primary)
    }
}

private struct TightLabel: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: Space.xs) { configuration.icon; configuration.title }
    }
}

/// 2 · Let it see the screen: put the board on this iPhone's Wi-Fi, then Screen Mirroring.
/// The page's one action is the bottom button (OnboardingView).
struct SeePage: View {
    @ObservedObject var app: AppModel
    @ObservedObject var board: BoardLink
    @Binding var ssid: String
    @Binding var password: String

    private var streaming: Bool { board.connected && board.status?.ap == "streaming" }
    private var joined: Bool { app.boardOnNetwork }
    private var failure: String? { if case .failed(let why) = app.boardWiFi { return why } else { return nil } }

    var body: some View {
        VStack(spacing: Space.xl) {
            PageHeader(mood: streaming ? .success : (app.boardWiFi.busy || joined) ? .listening : failure != nil ? .failure : .idle,
                       title: "Let Me See the Screen",
                       message: "Screen Mirroring goes to the board over your Wi-Fi.")
            Panel {
                CheckRow(title: "Connect the board to Wi-Fi", done: joined, active: !joined, waiting: app.boardWiFi.busy && !joined,
                         detail: joined ? nil : app.boardWiFi.busy ? "Joining can take up to 30 seconds — hang tight."
                            : failure.map { LocalizedStringKey($0) })
                Divider().padding(.vertical, Space.s)
                CheckRow(title: "Start Screen Mirroring", done: streaming, active: joined && !streaming)
            }
            if !joined {
                Panel { BoardWiFiFields(ssid: $ssid, password: $password) }
                VStack(alignment: .leading, spacing: Space.s) {
                    Label("Enter the Wi-Fi this iPhone is on. The name and password go to the board over Bluetooth.", systemImage: "wifi")
                    Label("The board only sees 2.4 GHz Wi-Fi.", systemImage: "antenna.radiowaves.left.and.right")
                    Label("Guest and office networks may keep devices apart; then mirroring can't find the board.", systemImage: "exclamationmark.triangle")
                }
                .font(.footnote).foregroundStyle(.secondary)
            } else if !streaming {
                Text("Now swipe down from the top-right corner and tap **Screen Mirroring → ESP32 Agent**.")
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                MiniPhoneDemo(kind: .controlCenter)
                VStack(spacing: Space.s) {
                    HStack(spacing: Space.s) {
                        ProgressView().controlSize(.small)
                        Text("Waiting for Screen Mirroring…")
                    }
                    Label("Keep the screen unlocked: locking stops mirroring.", systemImage: "lock.open")
                    Label("Not in the list? Keep this iPhone on the same Wi-Fi as the board.", systemImage: "wifi")
                }
                .font(.footnote).foregroundStyle(.secondary)
            }
        }
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: joined)
        .animation(.spring(response: 0.4, dampingFraction: 0.85), value: app.boardWiFi)
        .onChange(of: board.net?.ssid, initial: true) { _, name in
            if ssid.isEmpty, let name { ssid = name }
        }
    }
}

/// 3 · Let it act: AssistiveTouch (shown as a demo), then calibration on the grid board (bottom button).
struct ActPage: View {
    @ObservedObject var app: AppModel
    @ObservedObject var board: BoardLink
    @ObservedObject var calibrator: AutoCalibrator
    @ObservedObject var pointer: Pointer

    private var mouse: Bool { MockMode.on || board.status?.usb == "ok" }
    private var mirroring: Bool { MockMode.on || board.status?.ap == "streaming" }

    var body: some View {
        VStack(spacing: Space.xl) {
            PageHeader(mood: pointer.calibrated ? .success : calibrator.busy ? .acting : .idle,
                       title: "Let Me Use the Pointer",
                       message: "The board is a USB mouse. iOS shows its pointer only with AssistiveTouch on. Then I calibrate on a grid, like a 3D printer levelling its bed.")
            if !pointer.calibrated { MiniPhoneDemo(kind: .assistiveTouch) }
            Panel {
                CheckRow(title: "Mouse connected", done: mouse, active: !mouse, waiting: board.connected && !mouse)
                Divider().padding(.vertical, Space.s)
                CheckRow(title: "Screen Mirroring on", done: mirroring, active: mouse && !mirroring)
                Divider().padding(.vertical, Space.s)
                CheckRow(title: "Pointer calibrated", done: pointer.calibrated, active: mouse && mirroring && !pointer.calibrated,
                         waiting: calibrator.busy,
                         detail: pointer.quality.map { "Accuracy \(CalibrationText.percent($0))" })
            }
            if !pointer.calibrated {
                Label("Recommended: calibrate before first use.", systemImage: "scope")
                    .font(.footnote.weight(.semibold)).foregroundStyle(Color.accentColor)
                Text("Tip: in AssistiveTouch › Pointer Devices, set Tracking Speed to the middle and turn Pointer Animations off.")
                    .font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
            }
        }
    }
}

/// 4 · Give it a brain: an OpenAI-compatible vision model; Test Connection; a first task to try.
struct BrainPage: View {
    @ObservedObject var app: AppModel
    @ObservedObject var settings: Settings

    var body: some View {
        VStack(spacing: Space.xl) {
            PageHeader(mood: app.modelOK == true ? .success : app.modelOK == false ? .failure : .thinking,
                       title: "Give Me a Brain",
                       message: "Any OpenAI-compatible vision model works. The key stays in this iPhone's Keychain.")
            if MockMode.on {
                Panel { Label("Demo data simulates the model.", systemImage: "theatermasks") }
            } else {
                ModelFieldsPanel(settings: settings)
            }
            ModelTestTimingView(app: app)
            switch app.modelOK {
            case true?:
                Panel {
                    Label("Connected. Try this first:", systemImage: "checkmark.circle.fill").foregroundStyle(Palette.ok)
                    Text(verbatim: IntentStore.examples[0].prompt).font(.subheadline).foregroundStyle(.secondary)
                        .padding(.top, Space.s)
                }
            case false?:
                Label(app.modelError ?? String(localized: "Could not reach the model. Check the URL and key."), systemImage: "xmark.octagon.fill")
                    .foregroundStyle(Palette.stop).font(.subheadline)
            case nil:
                EmptyView()
            }
        }
    }
}

/// The model box of the Brain page: who decides each step (with the model folded into its row) when the build has
/// Jev, else endpoint and key with placeholders only; the model is picked from the endpoint (nothing personal is built in).
struct ModelFieldsPanel: View {
    @ObservedObject var settings: Settings

    var body: some View {
        if Trial.available {
            DecidersPanel(settings: settings)
        } else {
            fields
        }
    }

    private var fields: some View {
        VStack(spacing: Space.xs) {
            Panel { ProviderFields(settings: settings) }
            if let url = settings.provider.keyURL {
                Link("Get an API key", destination: url)
                    .font(.footnote).tint(.secondary).underline()
                    .frame(maxWidth: .infinity, minHeight: 44, alignment: .trailing)
            }
        }
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
    }
}
