// 一间房的操作权限，做成混入而不是每个卡片各写一遍 computed。
//
// 为什么：房间页上挂着一间房的卡片有五个（世界规则、集群设置、游戏端口、Klei 令牌、
// 远程指令），另外总览那一排房间卡和账号页的「房间归属」还要在同一批字段上摆标记。
// 判断散在这么多组件里的话，加第六个卡片必然漏一个——而漏掉的那个表现为"成员点保存，
// 接口回一句中文拒他"，看起来像面板在跟他打太极。
//
// 三条判断的口径全在 store.js（它只读后端给的 isAdmin / ownedByMe / canAssignOwner），
// 这里只负责把它们接到组件的 this 上。
import { roomWritable, readOnlyNote, canAssignOwner } from "./store.js";

export const RoomAccess = {
  computed: {
    // writable：这一间我改得动吗。注意它不等于"我是管理员"——成员在自己名下的房间里
    // 和管理员能做的一样多（除开归属本身）。
    writable() { return roomWritable(this.cluster); },
    // readOnlyNote：改不动时那句"为什么"。能改时是空串，模板里直接 v-if 就行。
    readOnlyNote() { return readOnlyNote(this.cluster); },
    canAssign() { return canAssignOwner(); },
  },
};
