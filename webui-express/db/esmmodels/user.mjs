import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class User extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

User.init(
    {
        email: DataTypes.STRING,
        hash: DataTypes.STRING,
    },
    {
        sequelize,
        modelName: "User",
    }
);

export default User;
