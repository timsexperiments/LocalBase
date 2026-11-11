import SwiftUI

// The host app UI test runners require. The tests drive Safari, not this.
@main
struct LBHostApp: App {
    var body: some Scene {
        WindowGroup { Text("LocalBase iOS check host") }
    }
}
