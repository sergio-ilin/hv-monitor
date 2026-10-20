using System;
using System.Runtime.InteropServices;
using System.Runtime.InteropServices.ComTypes;

public class ShortcutHelper {
    [ComImport]
    [Guid("00021401-0000-0000-C000-000000000046")]
    private class ShellLink {}

    [ComImport]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    [Guid("886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99")]
    private interface IPropertyStore {
        void GetCount(out uint cProps);
        void GetAt(uint iProp, out PROPERTYKEY pkey);
        void GetValue(ref PROPERTYKEY key, [Out] PROPVARIANT pv);
        void SetValue(ref PROPERTYKEY key, [In] ref PROPVARIANT pv);
        void Commit();
    }

    [StructLayout(LayoutKind.Sequential, Pack = 4)]
    private struct PROPERTYKEY {
        public Guid fmtid;
        public uint pid;
        public PROPERTYKEY(Guid guid, uint id) {
            fmtid = guid;
            pid = id;
        }
    }

    [StructLayout(LayoutKind.Explicit)]
    private struct PROPVARIANT {
        [FieldOffset(0)] public ushort vt;
        [FieldOffset(8)] public IntPtr pwszVal;
    }

    public static void SetAppId(string shortcutPath, string appId) {
        ShellLink link = new ShellLink();
        // STGM_READWRITE | STGM_SHARE_EXCLUSIVE = 0x00000002 | 0x00000010 = 0x12
        ((IPersistFile)link).Load(shortcutPath, 0x12);

        IPropertyStore propStore = (IPropertyStore)link;
        PROPERTYKEY pkey = new PROPERTYKEY(new Guid("9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3"), 5);

        PROPVARIANT pv = new PROPVARIANT();
        pv.vt = 31; // VT_LPWSTR
        pv.pwszVal = Marshal.StringToCoTaskMemUni(appId);

        try {
            propStore.SetValue(ref pkey, ref pv);
            propStore.Commit();
            ((IPersistFile)link).Save(shortcutPath, true);
        } finally {
            Marshal.FreeCoTaskMem(pv.pwszVal);
        }
    }

    public static void Main(string[] args) {
        if (args.Length >= 2) {
            SetAppId(args[0], args[1]);
            Console.WriteLine("Successfully set AppId: " + args[1]);
        }
    }
}
