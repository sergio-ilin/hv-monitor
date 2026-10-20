using System;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Net;
using System.Runtime.InteropServices;
using System.Threading;
using System.Windows.Forms;
using Microsoft.Web.WebView2.WinForms;
using Microsoft.Web.WebView2.Core;

namespace HyperVMonitor
{
    static class Program
    {
        [DllImport("shell32.dll", SetLastError = true)]
        private static extern void SetCurrentProcessExplicitAppUserModelID([MarshalAs(UnmanagedType.LPWStr)] string AppID);

        private const string AppId = "Sergio.HVMonitor";
        private const string ServerUrl = "http://127.0.0.1:18288";

        [STAThread]
        static void Main()
        {
            try {
                SetCurrentProcessExplicitAppUserModelID(AppId);
            } catch {}

            Application.EnableVisualStyles();
            Application.SetCompatibleTextRenderingDefault(false);
            Application.Run(new MainWindow());
        }

        public class MainWindow : Form
        {
            private WebView2 webView;

            public MainWindow()
            {
                Text = "Hyper-V Monitor";
                Size = new Size(1280, 880);
                MinimumSize = new Size(960, 600);
                StartPosition = FormStartPosition.CenterScreen;

                try {
                    string icoPath = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "icon.ico");
                    if (File.Exists(icoPath)) {
                        this.Icon = new Icon(icoPath);
                    }
                } catch {}

                webView = new WebView2 { Dock = DockStyle.Fill };
                Controls.Add(webView);

                this.FormClosing += MainWindow_FormClosing;

                InitWebView();
            }

            private async void InitWebView()
            {
                try {
                    EnsureBackendRunning();

                    string udata = Path.Combine(
                        Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
                        "HVMonitorProfile"
                    );
                    Directory.CreateDirectory(udata);

                    var env = await CoreWebView2Environment.CreateAsync(null, udata);
                    await webView.EnsureCoreWebView2Async(env);

                    // Optimize WebView2 settings for a standalone desktop app
                    webView.CoreWebView2.Settings.IsStatusBarEnabled = false;
                    webView.CoreWebView2.Settings.AreDefaultContextMenusEnabled = true;

                    webView.CoreWebView2.Navigate(ServerUrl);
                } catch (Exception ex) {
                    MessageBox.Show(
                        "Failed to initialize WebView2:\n" + ex.Message,
                        "Hyper-V Monitor",
                        MessageBoxButtons.OK,
                        MessageBoxIcon.Error
                    );
                }
            }

            private void EnsureBackendRunning()
            {
                for (int attempt = 0; attempt < 15; attempt++) {
                    try {
                        var req = (HttpWebRequest)WebRequest.Create(ServerUrl + "/api/system/info");
                        req.Timeout = 800;
                        using (var resp = req.GetResponse()) {
                            return; // Backend is responding!
                        }
                    } catch {
                        if (attempt == 0) {
                            string daemonPath = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "hv-monitor.exe");
                            if (File.Exists(daemonPath)) {
                                try {
                                    var psi = new ProcessStartInfo {
                                        FileName = daemonPath,
                                        Arguments = "-daemon",
                                        WorkingDirectory = AppDomain.CurrentDomain.BaseDirectory,
                                        UseShellExecute = true
                                    };
                                    Process.Start(psi);
                                } catch {
                                    try {
                                        Process.Start(daemonPath, "-daemon");
                                    } catch {}
                                }
                            }
                        }
                        Thread.Sleep(500);
                    }
                }
            }

            private void MainWindow_FormClosing(object sender, FormClosingEventArgs e)
            {
                try {
                    var req = (HttpWebRequest)WebRequest.Create(ServerUrl + "/api/exit");
                    req.Method = "GET";
                    req.Timeout = 800;
                    using (var resp = req.GetResponse()) {}
                } catch {}
            }
        }
    }
}
